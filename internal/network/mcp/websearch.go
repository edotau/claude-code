package mcp

// Stdlib-only stdio MCP server exposing `web_search` via Tavily; stdout carries only MCP JSON-RPC, logs go to stderr.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/vault"
)

const (
	defaultProtocolVersion = "2025-11-25"
	serverName             = "websearch"
	serverVersion          = "1.0.0"
	toolName               = "web_search"
	toolDescription        = "Search the web via Tavily for up-to-date public information, current events, " +
		"and recent data. Use when the question needs information beyond your training data."
	tavilyURL         = "https://api.tavily.com/search"
	tavilyExtractURL  = "https://api.tavily.com/extract"
	requestTimeout    = 20 * time.Second
	defaultMaxResults = 5
)

// keyName is the secret name Tavily's API key resolves under (the vault, or env).
const keyName = "TAVILY_API_KEY"

// mintHint points at where a fresh key comes from; keyHint names the fix when none is set.
const mintHint = "mint one at app.tavily.com → API keys"

func keyHint() string {
	return fmt.Sprintf("run `claude-code vault set %s` (%s)", keyName, mintHint)
}

// resolveKey returns the Tavily API key through the managed ladder (vault -> env). "" when unset.
func resolveKey() string {
	v, _, ok := vault.Get(keyName)
	if !ok {
		return ""
	}
	return v
}

// tavilyResult is one Tavily hit — the three fields the text render uses.
type tavilyResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// rpcRequest is one JSON-RPC 2.0 request. Missing "id" (checked separately) marks a notification.
type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func result(id json.RawMessage, res any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawID(id), "result": res}
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": rawID(id), "error": map[string]any{"code": code, "message": message}}
}

// rawID returns the id as-is, or JSON null when absent, so error responses always carry an id field.
func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return id
}

// toolError is an MCP tool-call result with isError=true. Tool-level failures are returned as
// results (not JSON-RPC errors) so the model sees and reacts to them rather than aborting the call.
func toolError(text string) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": true}
}

func toolText(text string) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}}
}

func toolDescriptor() map[string]any {
	return map[string]any{
		"name":        toolName,
		"description": toolDescription,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "The search query."},
				"max_results": map[string]any{"type": "integer",
					"description": fmt.Sprintf("Max results (default %d).", defaultMaxResults), "default": defaultMaxResults},
			},
			"required": []string{"query"},
		},
	}
}

// Searcher runs one web search. The default is Tavily; tests inject a fake to avoid network calls.
type Searcher func(ctx context.Context, query string, maxResults int) (string, error)

// Server holds the injectable search backend. Zero value uses the Tavily HTTP backend.
type Server struct {
	Search Searcher
}

// supportedProtocolVersions are the MCP revisions whose semantics this tools-only server implements.
// 2026-07-28 is deliberately ABSENT: it retires the initialize handshake for a stateless _meta core.
var supportedProtocolVersions = map[string]bool{
	"2024-11-05": true, "2025-03-26": true, "2025-06-18": true, "2025-11-25": true,
}

// negotiateProtocolVersion echoes the client's requested revision when supported, else falls back to
// our newest — the spec's negotiation rule, which a hardcoded reply silently violated.
func negotiateProtocolVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &p); err == nil && supportedProtocolVersions[p.ProtocolVersion] {
		return p.ProtocolVersion
	}
	return defaultProtocolVersion
}

// handle dispatches one request. Returns nil for notifications (no response per JSON-RPC spec).
func (s *Server) handle(ctx context.Context, req rpcRequest) map[string]any {
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		return result(req.ID, map[string]any{
			"protocolVersion": negotiateProtocolVersion(req.Params),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		})
	case "notifications/initialized":
		return nil
	case "ping":
		if isNotification {
			return nil
		}
		return result(req.ID, map[string]any{})
	case "tools/list":
		return result(req.ID, map[string]any{"tools": []map[string]any{toolDescriptor()}})
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	}
	if isNotification {
		return nil
	}
	return rpcError(req.ID, -32601, fmt.Sprintf("Method not found: %q", req.Method))
}

func (s *Server) handleToolsCall(ctx context.Context, req rpcRequest) map[string]any {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Query      string `json:"query"`
			MaxResults int    `json:"max_results"`
		} `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &p)
	if p.Name != toolName {
		return rpcError(req.ID, -32602, fmt.Sprintf("Unknown tool: %q", p.Name))
	}
	if strings.TrimSpace(p.Arguments.Query) == "" {
		return result(req.ID, toolError("`query` must be a non-empty string."))
	}
	maxResults := p.Arguments.MaxResults
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	search := s.Search
	if search == nil {
		search = tavilySearch
	}
	text, err := search(ctx, p.Arguments.Query, maxResults)
	if err != nil {
		return result(req.ID, toolError(err.Error()))
	}
	if strings.TrimSpace(text) == "" {
		return result(req.ID, toolError("No results found."))
	}
	return result(req.ID, toolText(text))
}

// Serve reads newline-delimited JSON-RPC requests from in, writes responses to out, until EOF.
// ctx bounds every search the loop dispatches, so a cancelled server does not sit out a hung fetch.
//
// Each request is handled on its OWN goroutine: Claude Code batches independent tool calls, and an
// inline handle() serialized N parallel searches into N back-to-back round-trips. JSON-RPC ids make
// out-of-order responses legal, so only the stdout writes need the mutex; EOF waits for the
// in-flight handlers, so a late response is never dropped by a closing client.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(out)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex // guards enc and writeErr
		writeErr error
	)
	send := func(resp map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		if writeErr == nil {
			writeErr = enc.Encode(resp)
		}
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			send(rpcError(nil, -32700, "Parse error"))
			continue
		}
		wg.Go(func() {
			if resp := s.handle(ctx, req); resp != nil {
				send(resp)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if writeErr != nil {
		return writeErr
	}
	return scanner.Err()
}

// Run wires stdin/stdout to a default (Tavily-backed) server. Entry point for the CLI command.
func Run(ctx context.Context) int {
	if resolveKey() == "" {
		fmt.Fprintf(os.Stderr, "websearch-mcp: no %s; web_search returns an error until set — %s\n", keyName, keyHint())
	}
	srv := &Server{}
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "websearch-mcp:", err)
		return 1
	}
	return 0
}

// OneShot runs a single Tavily search and writes the result text to out (Bash/skill entry point, no
// MCP handshake). Returns a non-zero exit code on error.
func OneShot(ctx context.Context, query string, maxResults int, search Searcher, out io.Writer) int {
	if strings.TrimSpace(query) == "" {
		fmt.Fprintln(os.Stderr, "web search: a query is required")
		return 2
	}
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	if search == nil {
		search = tavilySearch
	}
	text, err := search(ctx, query, maxResults)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	fmt.Fprintln(out, text)
	return 0
}

// tavilySearch is the default backend: POST the query and render the response as text. requestTimeout
// is this call's own bound, layered on the caller's ctx rather than replacing it.
func tavilySearch(ctx context.Context, query string, maxResults int) (string, error) {
	apiKey := resolveKey()
	if apiKey == "" {
		return "", fmt.Errorf("%s is not set — %s", keyName, keyHint())
	}
	body, _ := json.Marshal(map[string]any{
		"api_key":      apiKey,
		"query":        query,
		"max_results":  maxResults,
		"search_depth": "basic",
	})
	raw, err := postTavilySearch(ctx, body)
	if err != nil {
		return "", err
	}
	var payload struct {
		Answer  string         `json:"answer"`
		Results []tavilyResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("search with Tavily returned non-JSON payload (%v)", err)
	}
	enrichResults(ctx, payload.Results) // the Tavily deadline is spent; each fetch has its own bound
	return formatResults(payload.Answer, payload.Results), nil
}

// postTavilySearch POSTs body to the search endpoint under requestTimeout and returns the size-capped reply.
func postTavilySearch(ctx context.Context, body []byte) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, tavilyURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("could not build Tavily request (%v)", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code-websearch/"+serverVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach Tavily (%v)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search with Tavily failed (HTTP %d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, enrichMaxBody))
	if err != nil {
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
	reqCtx, cancel := context.WithTimeout(ctx, 2*requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, tavilyExtractURL, bytes.NewReader(body))
	if err != nil {
		return payload, fmt.Errorf("could not build Tavily request (%v)", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "claude-code-websearch/"+serverVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return payload, fmt.Errorf("could not reach Tavily (%v)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return payload, fmt.Errorf("extract with Tavily failed (HTTP %d)", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
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
	for _, depth := range []string{"basic", "advanced"} {
		res, err := tavilyExtract(ctx, apiKey, urls, depth)
		if err != nil {
			return err
		}
		urls = nil
		for _, r := range res.Results {
			content[r.URL] = r.RawContent
			delete(failed, r.URL)
		}
		for _, f := range res.Failed {
			failed[f.URL] = f.Error
			urls = append(urls, f.URL)
		}
		if len(urls) == 0 {
			return nil
		}
	}
	return nil
}

// vetExtractURLs splits urls into those safe to send to Tavily and a failure reason per refused URL.
func vetExtractURLs(ctx context.Context, urls []string) ([]string, map[string]string) {
	failed := map[string]string{}
	var send []string
	cache := map[string]string{}
	for _, raw := range urls {
		u, err := checkExtractURL(raw)
		switch {
		case err != nil:
			failed[raw] = err.Error()
		case !robotsAllowed(ctx, u, cache):
			failed[raw] = "robots.txt disallows"
		default:
			send = append(send, raw)
		}
	}
	return send, failed
}

// Extract reads each URL's page as markdown through Tavily and prints `# <url>` blocks to stdout.
// Exit 1 when every URL failed, else 0.
func Extract(ctx context.Context, urls []string) int {
	send, failed := vetExtractURLs(ctx, urls)
	content := map[string]string{}
	if len(send) > 0 {
		apiKey := resolveKey()
		if apiKey == "" {
			fmt.Fprintf(os.Stderr, "%s is not set — %s\n", keyName, keyHint())
			return 1
		}
		if err := fetchPages(ctx, apiKey, send, content, failed); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
	}
	ok := 0
	for _, raw := range urls {
		if msg, bad := failed[raw]; bad {
			fmt.Printf("# %s\nfailed: %s\n\n", raw, msg)
			continue
		}
		ok++
		fmt.Printf("# %s\n%s\n\n", raw, content[raw])
	}
	if ok == 0 {
		return 1
	}
	return 0
}

// Enrichment bounds: each hit's page fetch gets its own timeout, a byte cap on the read (a page
// tail past it cannot change the first enrichMaxChars anyway), and a character cap on what replaces
// the snippet — five enriched results stay under ~10KB of tool output.
const (
	enrichTimeout  = 5 * time.Second
	enrichMaxBody  = 256 << 10
	enrichMaxChars = 1800
)

// enrichResults APPENDS each page's own text after the Tavily snippet, fetched CONCURRENTLY so the
// whole pass costs one slowest-fetch, not a sum. Append, never replace: the snippet is Tavily's
// query-relevance extract while the page text is positional (top of the body) — a hit whose match
// is deep in the page would lose the relevant part outright. Best-effort per hit: any failure
// (timeout, non-HTML, non-200, empty extraction) keeps the snippet — the result never gets worse.
func enrichResults(ctx context.Context, results []tavilyResult) {
	var wg sync.WaitGroup
	for i := range results {
		r := &results[i]
		wg.Go(func() {
			if text := fetchPageText(ctx, r.URL); text != "" && text != r.Content {
				if r.Content == "" {
					r.Content = text
				} else {
					r.Content += "\n\n" + text
				}
			}
		})
	}
	wg.Wait()
}

// pageClient fetches result pages: loopback/private/link-local targets are refused at dial time (so redirects
// and DNS answers are covered too) and redirects are capped at 5 hops.
var pageClient = &http.Client{
	Transport:     &http.Transport{DialContext: (&net.Dialer{Control: rejectInternal}).DialContext},
	CheckRedirect: capRedirects,
}

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
	if !publicIP(net.ParseIP(host)) {
		return fmt.Errorf("refusing to fetch internal address %s", host)
	}
	return nil
}

// publicIP is false for nil and for loopback, private, link-local and unspecified addresses.
func publicIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

// robotsAllowed reports whether the origin's robots.txt permits u for "claude-code" or "*"; a missing or
// unreadable robots.txt allows. Parsing is the browser package's.
func robotsAllowed(ctx context.Context, u *url.URL, cache map[string]string) bool {
	origin := u.Scheme + "://" + u.Host
	body, ok := cache[origin]
	if !ok {
		body = fetchRobots(ctx, origin)
		cache[origin] = body
	}
	return !browser.RobotsBlocks(body, u.RequestURI())
}

// fetchRobots returns the origin's robots.txt body, or "" when it is missing or unreadable.
func fetchRobots(ctx context.Context, origin string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "claude-code-websearch/"+serverVersion)
	resp, err := pageClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return ""
	}
	return string(raw)
}

// fetchPageText GETs one result URL and reduces it to plain text; "" means "keep the snippet".
// Only http/https schemes are fetched — a search backend must not be able to point the enricher at
// file:, ftp:, or any other local scheme.
func fetchPageText(ctx context.Context, pageURL string) string {
	if !strings.HasPrefix(pageURL, "https://") && !strings.HasPrefix(pageURL, "http://") {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, enrichTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("User-Agent", "claude-code-websearch/"+serverVersion)
	cache := map[string]string{}
	if !robotsAllowed(ctx, req.URL, cache) {
		return ""
	}
	client := *pageClient
	client.CheckRedirect = robotsRedirectCheck(cache)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	return pageBodyText(resp)
}

// robotsRedirectCheck caps redirects and re-applies robots.txt at every hop.
func robotsRedirectCheck(cache map[string]string) func(*http.Request, []*http.Request) error {
	return func(next *http.Request, via []*http.Request) error {
		if err := capRedirects(next, via); err != nil {
			return err
		}
		if !robotsAllowed(next.Context(), next.URL, cache) {
			return fmt.Errorf("robots.txt disallows redirect to %s", next.URL)
		}
		return nil
	}
}

// textual reports whether a Content-Type is absent or names html/text.
func textual(ct string) bool {
	return ct == "" || strings.Contains(ct, "html") || strings.Contains(ct, "text")
}

// pageBodyText reduces a 200 text/HTML response to capped plain text; "" keeps the snippet.
func pageBodyText(resp *http.Response) string {
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	if !textual(resp.Header.Get("Content-Type")) {
		return "" // a PDF/image/binary hit keeps its snippet — no text to extract
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, enrichMaxBody))
	if err != nil {
		return "" // a body cut off mid-read keeps the snippet rather than appending a fragment
	}
	text := htmlToText(string(raw))
	if runes := []rune(text); len(runes) > enrichMaxChars {
		text = string(runes[:enrichMaxChars]) + "…"
	}
	return text
}

// Markup strippers for htmlToText: script/style/head subtrees vanish wholesale, every other tag
// becomes a space, and runs of whitespace collapse — crude, stdlib-only, and enough for a model to
// read; a failed extraction only ever falls back to the snippet.
// htmlRes lazily compiles the strippers, so a run that never fetches a web page pays no init cost.
var htmlRes = sync.OnceValue(func() (r struct{ drop, tag, spaceRun *regexp.Regexp }) {
	r.drop = regexp.MustCompile(`(?is)<(script|style|head)\b.*?</(script|style|head)>`)
	r.tag = regexp.MustCompile(`(?s)<[^>]*>`)
	r.spaceRun = regexp.MustCompile(`\s+`)
	return
})

func htmlToText(page string) string {
	page = htmlRes().drop.ReplaceAllString(page, " ")
	page = htmlRes().tag.ReplaceAllString(page, " ")
	page = html.UnescapeString(page)
	return strings.TrimSpace(htmlRes().spaceRun.ReplaceAllString(page, " "))
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
