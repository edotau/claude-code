// Stdlib-only stdio MCP server exposing `web_search` via Tavily. Only stdout carries MCP JSON-RPC;
// logging goes to stderr.
package websearch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/secrets"
)

const (
	defaultProtocolVersion = "2025-11-25"
	serverName             = "websearch"
	serverVersion          = "1.0.0"
	toolName               = "web_search"
	toolDescription        = "Search the web via Tavily for up-to-date public information, current events, " +
		"and recent data. Use when the question needs information beyond your training data."
	tavilyURL         = "https://api.tavily.com/search"
	requestTimeout    = 20 * time.Second
	defaultMaxResults = 5
)

// KeyName is the secret name Tavily's API key resolves under (keyring, env.d/secrets.env, or env).
const KeyName = "TAVILY_API_KEY"

// mintHint points at where a fresh key comes from; keyHint names the fix when none is set.
const mintHint = "mint one at app.tavily.com → API keys"

func keyHint() string {
	return fmt.Sprintf("run `claude-code secrets set %s` (%s)", KeyName, mintHint)
}

// resolveKey returns the Tavily API key through the managed ladder (keyring -> file -> env). "" when unset.
func resolveKey() string {
	v, _, ok := secrets.Get(KeyName)
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
		fmt.Fprintf(os.Stderr, "websearch-mcp: no %s; web_search returns an error until set — %s\n", KeyName, keyHint())
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
		return "", fmt.Errorf("%s is not set — %s", KeyName, keyHint())
	}
	body, _ := json.Marshal(map[string]any{
		"api_key":      apiKey,
		"query":        query,
		"max_results":  maxResults,
		"search_depth": "basic",
	})
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilyURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("could not build Tavily request (%v)", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code-websearch/"+serverVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("could not reach Tavily (%v)", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search with Tavily failed (HTTP %d)", resp.StatusCode)
	}
	var payload struct {
		Answer  string         `json:"answer"`
		Results []tavilyResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("search with Tavily returned non-JSON payload (%v)", err)
	}
	enrichResults(ctx, payload.Results)
	return formatResults(payload.Answer, payload.Results), nil
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" &&
		!strings.Contains(ct, "html") && !strings.Contains(ct, "text") {
		return "" // a PDF/image/binary hit keeps its snippet — no text to extract
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, enrichMaxBody))
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
