package websearch

// Stdlib-only stdio MCP server for `web_search` and `web_extract` via Tavily; stdout is JSON-RPC only, logs to stderr.

import (
	"bufio"
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

	"github.com/edotau/claude-code/internal/network/httpx"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

const (
	defaultProtocolVersion = "2025-11-25"
	serverName             = "websearch"
	serverVersion          = "1.0.0"
	userAgent              = "claude-code-websearch/" + serverVersion
	toolName               = "web_search"
	extractToolName        = "web_extract"
	extractToolDescription = "Read public web pages as markdown via Tavily; up to 20 URLs; honors robots.txt"
	maxExtractURLs         = 20
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

// resolveKey returns the Tavily API key (vault, then env); an unreadable vault is its own error, never "unset".
func resolveKey() (string, error) {
	v, _, err := vault.Lookup(keyName)
	if err != nil {
		return "", fmt.Errorf("%s: the vault could not be read (%w); export %s or fix vault access", keyName, err, keyName)
	}
	if v == "" {
		return "", fmt.Errorf("%s is not set — %s", keyName, keyHint())
	}
	return v, nil
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

// toolError is an isError tool result, not a JSON-RPC error, so the model sees the failure and reacts.
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

func extractDescriptor() map[string]any {
	return map[string]any{
		"name":        extractToolName,
		"description": extractToolDescription,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"urls": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
					"minItems": 1, "maxItems": maxExtractURLs, "description": "Public http(s) page URLs to read."},
			},
			"required": []string{"urls"},
		},
	}
}

// searcher runs one web search. The default is Tavily; tests inject a fake to avoid network calls.
type searcher func(ctx context.Context, query string, maxResults int) (string, error)

// server holds the injectable search backend. Zero value uses the Tavily HTTP backend.
type server struct {
	Search searcher
}

// supportedProtocolVersions are the MCP revisions served; 2026-07-28 is absent: it retires initialize.
var supportedProtocolVersions = map[string]bool{
	"2024-11-05": true, "2025-03-26": true, "2025-06-18": true, "2025-11-25": true,
}

// negotiateProtocolVersion echoes the client's revision when supported, else our newest (the spec's rule).
func negotiateProtocolVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &p); err == nil && supportedProtocolVersions[p.ProtocolVersion] {
		return p.ProtocolVersion
	}
	return defaultProtocolVersion
}

// handle dispatches one request. A notification (no id) gets no reply, whatever its method.
func (s *server) handle(ctx context.Context, req rpcRequest) map[string]any {
	if len(req.ID) == 0 {
		return nil
	}
	return s.dispatch(ctx, req)
}

func (s *server) dispatch(ctx context.Context, req rpcRequest) map[string]any {
	switch req.Method {
	case "initialize":
		return result(req.ID, map[string]any{
			"protocolVersion": negotiateProtocolVersion(req.Params),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		})
	case "ping":
		return result(req.ID, map[string]any{})
	case "tools/list":
		return result(req.ID, map[string]any{"tools": []map[string]any{toolDescriptor(), extractDescriptor()}})
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	case "notifications/initialized":
		return nil
	}
	return rpcError(req.ID, -32601, fmt.Sprintf("Method not found: %q", req.Method))
}

func (s *server) handleToolsCall(ctx context.Context, req rpcRequest) map[string]any {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Query      string   `json:"query"`
			MaxResults int      `json:"max_results"`
			URLs       []string `json:"urls"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return result(req.ID, toolError(fmt.Sprintf("invalid tool arguments: %v", err)))
	}
	if p.Name == extractToolName {
		return result(req.ID, extractToolResult(ctx, p.Arguments.URLs))
	}
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
		return result(req.ID, toolError("websearch: "+err.Error()))
	}
	if strings.TrimSpace(text) == "" {
		return result(req.ID, toolError("No results found."))
	}
	return result(req.ID, toolText(text))
}

// Serve answers newline-delimited JSON-RPC from in to out until EOF; ctx bounds every search.
// Gotcha: one goroutine per request (Claude Code batches tool calls); only writes lock, and EOF waits for in-flight.
func (s *server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	reader := bufio.NewReaderSize(in, 64*1024)
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
	var readErr error
	for {
		raw, tooLong, err := readLine(reader, maxLineBytes)
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
		if tooLong {
			send(rpcError(nil, -32700, "Parse error: line too long"))
			continue
		}
		req, ok := parseRequest(raw, send)
		if !ok {
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
	return readErr
}

// parseRequest decodes one line; ok is false for a blank line (skipped) or bad JSON (answered via send).
func parseRequest(raw []byte, send func(map[string]any)) (req rpcRequest, ok bool) {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return req, false
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		send(rpcError(nil, -32700, "Parse error"))
		return req, false
	}
	return req, true
}

// maxLineBytes caps one JSON-RPC line; a longer line is drained and answered with a parse error.
const maxLineBytes = 4 * 1024 * 1024

// readLine returns the next line without its newline; a line over max is drained and flagged tooLong.
func readLine(r *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > max {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch err {
		case bufio.ErrBufferFull:
			continue
		case nil:
			return line, tooLong, nil
		case io.EOF:
			if len(line) > 0 || tooLong {
				return line, tooLong, nil
			}
		}
		return nil, false, err
	}
}

// run wires stdin/stdout to a default (Tavily-backed) server. Entry point for the CLI command.
func run(ctx context.Context) int {
	if _, err := resolveKey(); err != nil {
		fmt.Fprintf(os.Stderr, "websearch: %v; web_search returns an error until fixed\n", err)
	}
	srv := &server{}
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		return helptext.Fail("network websearch: %v", err)
	}
	return 0
}

// oneShot runs one Tavily search and writes the text to out (no MCP handshake); non-zero exit on error.
func oneShot(ctx context.Context, query string, maxResults int, search searcher, out io.Writer) int {
	if strings.TrimSpace(query) == "" {
		return helptext.FailUsage("network websearch: a query is required")
	}
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	if search == nil {
		search = tavilySearch
	}
	text, err := search(ctx, query, maxResults)
	if err != nil {
		return helptext.Fail("network websearch: %v", err)
	}
	if _, err := fmt.Fprintln(out, text); err != nil {
		return helptext.Fail("network websearch: could not write result: %v", err)
	}
	return 0
}

// tavilySearch is the default backend; requestTimeout is layered on the caller's ctx, not replacing it.
func tavilySearch(ctx context.Context, query string, maxResults int) (string, error) {
	apiKey, err := resolveKey()
	if err != nil {
		return "", err
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
	return postTavily(ctx, requestTimeout, tavilyURL, "search", "", body, searchMaxBody)
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

// tavilyExtract is the HTTP seam for extractURLs; tests swap it to avoid the network.
var tavilyExtract = postTavilyExtract

func postTavilyExtract(ctx context.Context, apiKey string, urls []string, depth string) (extractResponse, error) {
	var payload extractResponse
	body, _ := json.Marshal(map[string]any{"urls": urls, "format": "markdown", "extract_depth": depth})
	raw, err := postTavily(ctx, 2*requestTimeout, tavilyExtractURL, "extract", apiKey, body, 16<<20)
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
			content[normURL(r.URL)] = r.RawContent
			delete(failed, normURL(r.URL))
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
	dead := map[string]bool{} // origins whose robots.txt could not be read: one timeout per origin, not per URL
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
func extractToolResult(ctx context.Context, urls []string) map[string]any {
	if len(urls) == 0 || len(urls) > maxExtractURLs {
		return toolError(fmt.Sprintf("`urls` must hold 1-%d URLs (got %d).", maxExtractURLs, len(urls)))
	}
	text, ok, err := renderExtract(ctx, urls)
	if err != nil {
		return toolError("websearch: " + err.Error())
	}
	if ok == 0 {
		return toolError(text)
	}
	return toolText(text)
}

// renderExtract reads each URL as markdown via Tavily into `# <url>` blocks; an error means nothing rendered.
func renderExtract(ctx context.Context, urls []string) (string, int, error) {
	send, failed := vetExtractURLs(ctx, urls)
	content := map[string]string{}
	if len(send) > 0 {
		apiKey, err := resolveKey()
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
		msg, bad := failed[raw]
		if !bad {
			msg, bad = failed[normURL(raw)]
		}
		if !bad && content[normURL(raw)] == "" {
			msg, bad = "no content returned", true
		}
		if bad {
			fmt.Fprintf(&sb, "# %s\nfailed: %s\n\n", raw, msg)
			continue
		}
		ok++
		fmt.Fprintf(&sb, "# %s\n%s\n\n", raw, content[normURL(raw)])
	}
	return sb.String(), ok, nil
}

// extractURLs prints renderExtract's `# <url>` blocks to stdout. Exit 1 when every URL failed, else 0.
func extractURLs(ctx context.Context, urls []string) int {
	if len(urls) > maxExtractURLs { // Tavily rejects the whole call over its cap, as the MCP path does
		return helptext.Fail("network websearch --extract: at most %d URLs (got %d)", maxExtractURLs, len(urls))
	}
	text, ok, err := renderExtract(ctx, urls)
	if err != nil {
		return helptext.Fail("network websearch --extract: %v", err)
	}
	if _, err := fmt.Print(text); err != nil {
		return helptext.Fail("network websearch --extract: could not write result: %v", err)
	}
	if ok == 0 {
		return 1
	}
	return 0
}

// Enrichment bounds: a per-fetch timeout, a read byte cap, and a char cap keeping five hits under ~10KB.
const (
	enrichMaxBody  = 256 << 10
	searchMaxBody  = 16 << 20 // the Tavily search reply, sized like the extract reply
	enrichMaxChars = 1800
)

// enrichTimeout is a var so tests can shorten it.
var enrichTimeout = 5 * time.Second

// enrichResults appends each page's text after its snippet, fetched concurrently; any failure keeps the snippet.
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

// pageClient refuses loopback/private/link-local targets at dial time (covers redirects and DNS) and caps 5 hops.
var pageClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Control: rejectInternal, Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
	},
	CheckRedirect: capRedirects,
	Timeout:       requestTimeout,
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

// publicIP is false for nil, loopback, private, CGNAT (100.64/10), 0.0.0.0/8, link-local, multicast, unspecified.
func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsMulticast() ||
		ip.IsUnspecified() {
		return false
	}
	return !reservedV4(ip.To4())
}

// reservedV4 is true for an IPv4 in 0.0.0.0/8 or CGNAT 100.64.0.0/10; nil (not IPv4) is false.
func reservedV4(v4 net.IP) bool {
	return v4 != nil && (v4[0] == 0 || (v4[0] == 100 && v4[1]&0xC0 == 64))
}

// fetchPageText GETs one http(s) result URL as plain text; "" keeps the snippet; other schemes are refused.
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
	req.Header.Set("User-Agent", userAgent)
	if !robotsAllowed(ctx, req.URL, nil) {
		return ""
	}
	client := *pageClient
	client.CheckRedirect = robotsRedirectCheck
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	return pageBodyText(resp)
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

// htmlRes lazily compiles htmlToText's strippers: drop script/style/head, tags to spaces, collapse whitespace.
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
