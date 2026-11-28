// Package websearch implements the Tavily web search, page extraction and stdio MCP server.
package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/httpx"
	"github.com/edotau/claude-code/internal/platform/mcpstdio"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/platform/vault"
)

const (
	serverName      = "websearch"
	serverVersion   = "1.0.0"
	userAgent       = "claude-code-websearch/" + serverVersion
	toolName        = "web_search"
	extractToolName = "web_extract"
	// MaxExtractURLs is the maximum number of URLs one extract call accepts.
	MaxExtractURLs      = 20
	extractPageCapRunes = 12000 // per-page bound on extract output
	tavilyURL           = "https://api.tavily.com/search"
	tavilyExtractURL    = "https://api.tavily.com/extract"
	requestTimeout      = 20 * time.Second
	// DefaultMaxResults is the default number of search hits returned when maxResults <= 0.
	DefaultMaxResults = 5
)

// keyName is the secret name Tavily's API key resolves under (the vault, or env).
const keyName = "TAVILY_API_KEY"

// mintHint points at where a fresh key comes from.
const mintHint = "mint one at app.tavily.com → API keys"

// ResolveKey returns the Tavily API key (vault, then env); an unreadable vault is its own error, never "unset".
func ResolveKey() (string, error) {
	v, _, err := vault.Lookup(keyName)
	if err != nil {
		return "", fmt.Errorf("%s: the vault could not be read (%w); export %s or fix vault access", keyName, err, keyName)
	}
	if v == "" {
		return "", fmt.Errorf("%s is not set — run `claude-code vault set secrets %s` (%s)", keyName, keyName, mintHint)
	}
	return v, nil
}

// tavilyResult is one Tavily hit — the three fields the text render uses.
type tavilyResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// Searcher runs one web search. The default is Tavily; tests inject a fake to avoid network calls.
type Searcher func(ctx context.Context, query string, maxResults int) (string, error)

// Server holds the injectable search backend. Zero value uses the Tavily HTTP backend.
type Server struct {
	Search Searcher
}

// tools builds the two MCP tools; search falls back to the Tavily backend.
func (s *Server) tools() []mcpstdio.Tool {
	return []mcpstdio.Tool{{
		Name:        toolName,
		Description: assets.WebSearchDescription(),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": assets.WebSearchQueryDescription()},
				"max_results": map[string]any{"type": "integer",
					"description": fmt.Sprintf(assets.WebSearchMaxResultsDescription(), DefaultMaxResults), "default": DefaultMaxResults},
			},
			"required": []string{"query"},
		},
		Call: s.searchCall,
	}, {
		Name:        extractToolName,
		Description: assets.WebExtractDescription(),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"urls": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"minItems": 1, "maxItems": MaxExtractURLs, "description": assets.WebExtractURLsDescription()},
			},
			"required": []string{"urls"},
		},
		Call: extractToolCall,
	}}
}

func (s *Server) searchCall(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid tool arguments: %v", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "", toolFailure("`query` must be a non-empty string.")
	}
	if args.MaxResults <= 0 {
		args.MaxResults = DefaultMaxResults
	}
	search := s.Search
	if search == nil {
		search = SearchText
	}
	text, err := search(ctx, args.Query, args.MaxResults)
	if err != nil {
		return "", errors.New("websearch: " + err.Error())
	}
	if strings.TrimSpace(text) == "" {
		return "", toolFailure("No results found.")
	}
	return text, nil
}

func extractToolCall(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		URLs []string `json:"urls"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid tool arguments: %v", err)
	}
	return extractToolResult(ctx, args.URLs)
}

// Serve runs the stdio MCP loop over the two web tools.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return (&mcpstdio.Server{Name: serverName, Version: serverVersion, Tools: s.tools()}).Serve(ctx, in, out)
}

// SearchText is the default backend; requestTimeout is layered on the caller's ctx, not replacing it.
func SearchText(ctx context.Context, query string, maxResults int) (string, error) {
	answer, results, err := fetchResults(ctx, query, maxResults, nil, nil)
	if err != nil {
		return "", err
	}
	return formatResults(answer, results), nil
}

// Hit is one search result for a caller outside the MCP server.
type Hit struct{ Title, URL, Snippet string }

// Search runs one Tavily search, limited to the allowed domains and away from the blocked ones.
func Search(ctx context.Context, query string, maxResults int, allowed, blocked []string) ([]Hit, error) {
	_, results, err := fetchResults(ctx, query, maxResults, allowed, blocked)
	hits := make([]Hit, 0, len(results))
	for _, r := range results {
		hits = append(hits, Hit{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return hits, err
}

// fetchResults posts one Tavily search and decodes the answer and results.
func fetchResults(ctx context.Context, query string, maxResults int, allowed, blocked []string) (string, []tavilyResult, error) {
	apiKey, err := ResolveKey()
	if err != nil {
		return "", nil, err
	}
	req := map[string]any{"api_key": apiKey, "query": query, "max_results": maxResults, "search_depth": "basic"}
	if len(allowed) > 0 {
		req["include_domains"] = allowed
	}
	if len(blocked) > 0 {
		req["exclude_domains"] = blocked
	}
	body, _ := json.Marshal(req)
	raw, err := postTavily(ctx, requestTimeout, tavilyURL, "search", "", body, tavilyMaxBody)
	if err != nil {
		return "", nil, err
	}
	var payload struct {
		Answer  string         `json:"answer"`
		Results []tavilyResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", nil, fmt.Errorf("search with Tavily returned non-JSON payload (%v)", err)
	}
	return payload.Answer, payload.Results, nil
}

// postTavily POSTs JSON body (a Bearer apiKey when set) under timeout and returns the reply capped at limit bytes.
func postTavily(ctx context.Context, timeout time.Duration, endpoint, what, apiKey string, body []byte, limit int64) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hdr := http.Header{"Content-Type": {"application/json"}, "User-Agent": {userAgent}}
	if apiKey != "" {
		hdr.Set("Authorization", "Bearer "+apiKey)
	}
	status, raw, _, err := httpx.Do(reqCtx, httpx.Call{Client: http.DefaultClient, Method: http.MethodPost, URL: endpoint,
		Header: hdr, Body: body, Limit: limit})
	switch {
	case status == 0:
		return nil, fmt.Errorf("could not reach Tavily (%v)", err)
	case status != http.StatusOK:
		return nil, fmt.Errorf("%s with Tavily failed (HTTP %d)", what, status)
	case errors.Is(err, httpx.ErrTooLarge):
		return nil, fmt.Errorf("tavily %s response exceeds %d bytes", what, limit)
	case err != nil:
		return nil, fmt.Errorf("could not read Tavily response (%v)", err)
	}
	return raw, nil
}

// extractResponse is the slice of Tavily's /extract reply the renderer uses.
type extractResponse struct {
	Results []struct {
		URL        string `json:"url"`
		RawContent string `json:"raw_content"`
	} `json:"results"`
	Failed []struct {
		URL   string `json:"url"`
		Error string `json:"error"`
	} `json:"failed_results"`
}

// tavilyExtract is the HTTP seam for Extract; tests swap it to avoid the network.
var tavilyExtract = postTavilyExtract

func postTavilyExtract(ctx context.Context, apiKey string, urls []string, depth string) (extractResponse, error) {
	var payload extractResponse
	body, _ := json.Marshal(map[string]any{"urls": urls, "format": "markdown", "extract_depth": depth})
	raw, err := postTavily(ctx, 2*requestTimeout, tavilyExtractURL, "extract", apiKey, body, tavilyMaxBody)
	if err != nil {
		return payload, err
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("extract with Tavily returned non-JSON payload (%v)", err)
	}
	return payload, nil
}

// checkExtractURL refuses non-http(s) URLs and literal internal hosts before anything is sent to Tavily.
func checkExtractURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("only http(s) URLs are supported")
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil, fmt.Errorf("refusing to fetch internal address %s", host)
	}
	if net.ParseIP(host) != nil {
		if err := rejectInternal("", net.JoinHostPort(host, "80"), nil); err != nil {
			return nil, err
		}
	}
	return u, nil
}

// fetchPages extracts at basic depth, then retries Tavily's failures once at advanced depth (bot-protected pages).
func fetchPages(ctx context.Context, apiKey string, urls []string, content, failed map[string]string) error {
	sent := map[string]string{}
	for _, u := range urls {
		sent[normURL(u)] = u
	}
	for _, depth := range []string{"basic", "advanced"} {
		res, err := tavilyExtract(ctx, apiKey, urls, depth)
		if err != nil {
			if depth == "basic" {
				return err
			}
			for _, u := range urls { // a failed retry keeps the basic pages; only the retried URLs fail
				failed[normURL(u)] = err.Error()
			}
			return nil
		}
		urls = nil
		for _, r := range res.Results {
			k := normURL(r.URL)
			content[k] = r.RawContent
			delete(failed, k)
		}
		for _, f := range res.Failed {
			k := normURL(f.URL)
			failed[k] = f.Error
			if orig, ok := sent[k]; ok {
				urls = append(urls, orig)
			} else {
				urls = append(urls, f.URL)
			}
		}
		if len(urls) == 0 {
			return nil
		}
	}
	return nil
}

// normURL canonicalises a URL (lowercase scheme/host, no fragment, "/" path) to match Tavily's echoed URL.
func normURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Scheme, u.Host, u.Fragment = strings.ToLower(u.Scheme), strings.ToLower(u.Host), ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

// vetExtractURLs splits urls into those safe to send to Tavily and a failure reason per refused URL.
func vetExtractURLs(ctx context.Context, urls []string) ([]string, map[string]string) {
	failed := map[string]string{}
	dead := warmRobots(ctx, urls) // origins whose robots.txt could not be read: one timeout per origin, not per URL
	var send []string
	for _, raw := range urls {
		u, err := checkExtractURL(raw)
		switch {
		case err != nil:
			failed[raw] = err.Error()
		case !robotsAllowed(ctx, u, dead):
			failed[raw] = "robots.txt disallows or could not be read"
		default:
			send = append(send, raw)
		}
	}
	return send, failed
}

// extractToolResult validates urls like the CLI, then renders pages; a key/fetch error or all-failed is a tool error.
func extractToolResult(ctx context.Context, urls []string) (string, error) {
	if len(urls) == 0 || len(urls) > MaxExtractURLs {
		return "", toolFailure(fmt.Sprintf("`urls` must hold 1-%d URLs (got %d).", MaxExtractURLs, len(urls)))
	}
	text, ok, err := Extract(ctx, urls)
	if err != nil {
		return "", errors.New("websearch: " + err.Error())
	}
	if ok == 0 {
		return "", errors.New(text)
	}
	return text, nil
}

// Extract reads each URL as markdown via Tavily into `# <url>` blocks; an error means nothing rendered.
func Extract(ctx context.Context, urls []string) (string, int, error) {
	send, failed := vetExtractURLs(ctx, urls)
	content := map[string]string{}
	if len(send) > 0 {
		apiKey, err := ResolveKey()
		if err != nil {
			return "", 0, err
		}
		if err := fetchPages(ctx, apiKey, send, content, failed); err != nil {
			return "", 0, err
		}
	}
	var sb strings.Builder
	ok := 0
	for _, raw := range urls {
		k := normURL(raw)
		msg, bad := failed[raw]
		if !bad {
			msg, bad = failed[k]
		}
		if !bad && content[k] == "" {
			msg, bad = "no content returned", true
		}
		if bad {
			fmt.Fprintf(&sb, "# %s\nfailed: %s\n\n", raw, msg)
			continue
		}
		ok++
		fmt.Fprintf(&sb, "# %s\n%s\n\n", raw, capExtractPage(content[k]))
	}
	return sb.String(), ok, nil
}

// capExtractPage keeps a page's first extractPageCapRunes runes and ends with a one-line truncation notice.
func capExtractPage(page string) string {
	total := textutil.RuneLen(page)
	if total <= extractPageCapRunes {
		return page
	}
	note := fmt.Sprintf("\n…[truncated: %d of %d chars shown; pass a narrower URL or fetch the section you need]",
		extractPageCapRunes, total)
	return textutil.CapRunes(page, extractPageCapRunes, note)
}

// tavilyMaxBody caps a Tavily search or extract reply.
const tavilyMaxBody = 16 << 20

// robotsTimeout bounds each robots.txt fetch; a var so tests can shorten it.
var robotsTimeout = 5 * time.Second

// pageClient refuses loopback/private/link-local targets at dial time (covers redirects and DNS) and caps 5 hops.
var pageClient = httpx.NewClient(requestTimeout, &http.Transport{
	DialContext:           (&net.Dialer{Control: rejectInternal, Timeout: 10 * time.Second}).DialContext,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 10 * time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConnsPerHost:   4,
}, capRedirects)

func capRedirects(_ *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	return nil
}

func rejectInternal(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if textutil.IsNonPublicIP(net.ParseIP(host), true) {
		return fmt.Errorf("refusing to fetch internal address %s", host)
	}
	return nil
}

// formatResults renders a Tavily response as a readable text block (answer first, then results).
func formatResults(answer string, results []tavilyResult) string {
	var blocks []string
	if strings.TrimSpace(answer) != "" {
		blocks = append(blocks, "Answer: "+answer)
	}
	for _, r := range results {
		title := r.Title
		if title == "" {
			title = "(no title)"
		}
		blocks = append(blocks, fmt.Sprintf("%s\n%s\n%s", title, r.URL, r.Content))
	}
	if len(blocks) == 0 {
		return "No results found."
	}
	return strings.Join(blocks, "\n\n")
}

// toolFailure is a tool error whose text reaches the client verbatim, full stop included.
type toolFailure string

func (e toolFailure) Error() string { return string(e) }
