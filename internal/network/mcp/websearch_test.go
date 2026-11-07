package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// drive runs the server over a scripted set of request lines and returns the decoded responses.
func drive(t *testing.T, srv *Server, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := srv.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resps []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("response not JSON: %q: %v", l, err)
		}
		resps = append(resps, m)
	}
	// Concurrent dispatch answers out of order by design; ids are the contract, so tests index a
	// deterministic id-sorted view (a nil id — parse errors — sorts first).
	sort.Slice(resps, func(i, j int) bool {
		fi, _ := resps[i]["id"].(float64)
		fj, _ := resps[j]["id"].(float64)
		return fi < fj
	})
	return resps
}

func TestInitializeAndToolsList(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "unused", nil }}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	)
	// notifications/initialized yields NO response, so we expect exactly 2 responses.
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2 (notification must not respond)", len(resps))
	}
	si, _ := resps[0]["result"].(map[string]any)
	if si["protocolVersion"] != defaultProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", si["protocolVersion"], defaultProtocolVersion)
	}
	list, _ := resps[1]["result"].(map[string]any)
	tools, _ := list["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1", tools)
	}
	if tool, _ := tools[0].(map[string]any); tool["name"] != toolName {
		t.Errorf("tool name = %v, want %s", tool["name"], toolName)
	}
}

func TestToolsCallSuccess(t *testing.T) {
	called := ""
	srv := &Server{Search: func(_ context.Context, q string, _ int) (string, error) {
		called = q
		return "RESULT for " + q, nil
	}}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"golang","max_results":3}}}`,
	)
	if called != "golang" {
		t.Errorf("searcher got query %q, want golang", called)
	}
	res, _ := resps[0]["result"].(map[string]any)
	if res["isError"] == true {
		t.Errorf("unexpected isError: %v", res)
	}
	content, _ := res["content"].([]any)
	first, _ := content[0].(map[string]any)
	if !strings.Contains(first["text"].(string), "RESULT for golang") {
		t.Errorf("content = %v, want the search result text", first["text"])
	}
}

func TestToolsCallEmptyQuery(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "x", nil }}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"  "}}}`,
	)
	res, _ := resps[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Errorf("empty query should be a tool error, got %v", res)
	}
}

func TestToolsCallSearcherError(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "", errString("boom") }}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`,
	)
	// A backend error is a tool error (isError=true result), not a JSON-RPC protocol error.
	if _, isRPCErr := resps[0]["error"]; isRPCErr {
		t.Errorf("backend error must be a tool result, not a JSON-RPC error: %v", resps[0])
	}
	res, _ := resps[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Errorf("searcher error should surface as isError result, got %v", res)
	}
}

func TestUnknownTool(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "x", nil }}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
	)
	if _, ok := resps[0]["error"]; !ok {
		t.Errorf("unknown tool should be a JSON-RPC error, got %v", resps[0])
	}
}

func TestUnknownMethodNotificationIgnored(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "x", nil }}
	// A notification (no id) for an unknown method must produce no response.
	resps := drive(t, srv, `{"jsonrpc":"2.0","method":"some/notify"}`)
	if len(resps) != 0 {
		t.Errorf("notification should yield no response, got %v", resps)
	}
}

func TestParseError(t *testing.T) {
	srv := &Server{}
	resps := drive(t, srv, `{not json`)
	if _, ok := resps[0]["error"]; !ok {
		t.Errorf("malformed line should yield a parse error, got %v", resps[0])
	}
}

func TestOneShotSuccess(t *testing.T) {
	var out strings.Builder
	got := ""
	code := OneShot(context.Background(), "golang generics", 3, func(_ context.Context, q string, n int) (string, error) {
		got = q
		if n != 3 {
			t.Errorf("max_results = %d, want 3", n)
		}
		return "the answer", nil
	}, &out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got != "golang generics" {
		t.Errorf("query = %q, want golang generics", got)
	}
	if strings.TrimSpace(out.String()) != "the answer" {
		t.Errorf("output = %q, want 'the answer'", out.String())
	}
}

func TestOneShotEmptyQuery(t *testing.T) {
	var out strings.Builder
	search := func(context.Context, string, int) (string, error) { return "x", nil }
	code := OneShot(context.Background(), "   ", 5, search, &out)
	if code != 2 {
		t.Errorf("empty query exit = %d, want 2", code)
	}
	if out.String() != "" {
		t.Errorf("empty query should print nothing to out, got %q", out.String())
	}
}

func TestOneShotDefaultsMaxResults(t *testing.T) {
	var out strings.Builder
	OneShot(context.Background(), "q", 0, func(_ context.Context, _ string, n int) (string, error) {
		if n != 5 {
			t.Errorf("default max_results = %d, want 5", n)
		}
		return "ok", nil
	}, &out)
}

func TestOneShotSearcherError(t *testing.T) {
	var out strings.Builder
	search := func(context.Context, string, int) (string, error) { return "", errString("boom") }
	code := OneShot(context.Background(), "q", 5, search, &out)
	if code != 1 {
		t.Errorf("searcher error exit = %d, want 1", code)
	}
}

// Cancellability is worthless unless the server's ctx actually reaches the backend, so assert the
// searcher observes the caller's cancellation rather than a fresh background context.
func TestServeThreadsContextToSearcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var seen error
	srv := &Server{Search: func(c context.Context, _ string, _ int) (string, error) {
		seen = c.Err()
		return "text", nil
	}}
	var out strings.Builder
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"q"}}}`
	if err := srv.Serve(ctx, strings.NewReader(line+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if !errors.Is(seen, context.Canceled) {
		t.Errorf("searcher ctx.Err() = %v, want context.Canceled", seen)
	}
}

// The Bash/skill entry point carries the same guarantee as Serve.
func TestOneShotThreadsContextToSearcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var seen error
	var out strings.Builder
	if code := OneShot(ctx, "q", 5, func(c context.Context, _ string, _ int) (string, error) {
		seen = c.Err()
		return "ok", nil
	}, &out); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !errors.Is(seen, context.Canceled) {
		t.Errorf("searcher ctx.Err() = %v, want context.Canceled", seen)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestInitializeNegotiatesProtocolVersion pins the MCP negotiation contract: the server echoes the
// client's requested revision when it supports it, and otherwise falls back to its newest.
func TestInitializeNegotiatesProtocolVersion(t *testing.T) {
	for name, tc := range map[string]struct{ params, want string }{
		"echoes a supported revision":     {`{"protocolVersion":"2025-06-18"}`, "2025-06-18"},
		"echoes the newest supported":     {`{"protocolVersion":"2025-11-25"}`, "2025-11-25"},
		"echoes the oldest supported":     {`{"protocolVersion":"2024-11-05"}`, "2024-11-05"},
		"absent version falls back":       {`{}`, defaultProtocolVersion},
		"unknown future version falls to": {`{"protocolVersion":"2099-01-01"}`, defaultProtocolVersion},
		"garbage version falls back":      {`{"protocolVersion":"not-a-version"}`, defaultProtocolVersion},
	} {
		t.Run(name, func(t *testing.T) {
			srv := &Server{Search: func(context.Context, string, int) (string, error) { return "unused", nil }}
			resps := drive(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+tc.params+`}`)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			res, _ := resps[0]["result"].(map[string]any)
			if got := res["protocolVersion"]; got != tc.want {
				t.Errorf("protocolVersion = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestDefaultProtocolVersionIsHonest guards against advertising a revision whose semantics this
// tools-only server does not implement: 2026-07-28 retired initialize and requires _meta routing.
func TestDefaultProtocolVersionIsHonest(t *testing.T) {
	if defaultProtocolVersion != "2025-11-25" {
		t.Errorf("defaultProtocolVersion = %s, want 2025-11-25 (newest revision whose semantics we implement)", defaultProtocolVersion)
	}
	if supportedProtocolVersions[defaultProtocolVersion] != true {
		t.Errorf("defaultProtocolVersion %s missing from supportedProtocolVersions", defaultProtocolVersion)
	}
	if supportedProtocolVersions["2026-07-28"] {
		t.Error("must not advertise 2026-07-28: stateless core, no initialize handshake, _meta per-request routing unimplemented")
	}
}

// TestServeDispatchesToolCallsConcurrently: Claude Code batches independent tool calls, so two
// in-flight searches must OVERLAP — the barrier backend releases only when both have arrived,
// which deadlocks (and times the test out) if Serve still handles requests inline.
func TestServeDispatchesToolCallsConcurrently(t *testing.T) {
	var inFlight atomic.Int32
	release := make(chan struct{})
	srv := &Server{Search: func(ctx context.Context, q string, n int) (string, error) {
		if inFlight.Add(1) == 2 {
			close(release) // both arrived: overlap proven
		}
		select {
		case <-release:
			return "answer to " + q, nil
		case <-time.After(5 * time.Second):
			return "", errors.New("no overlap: the second call never started")
		}
	}}
	resps := drive(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"a"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"b"}}}`,
	)
	if len(resps) != 2 {
		t.Fatalf("got %d responses, want 2", len(resps))
	}
	for i, want := range []string{"answer to a", "answer to b"} {
		res, _ := resps[i]["result"].(map[string]any)
		if !strings.Contains(fmt.Sprint(res), want) {
			t.Errorf("resps[%d] missing %q: %v", i, want, resps[i])
		}
	}
}

// enrichTestPage serves a good HTML page, a binary one, a hanging one (until slow closes) and 404s otherwise.
func enrichTestPage(slow chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/good":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><head><style>p{}</style></head><body><h1>Real &amp; Deep</h1><script>x()</script><p>page body text</p></body></html>`)
		case "/binary":
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "%PDF-1.7")
		case "/slow":
			<-slow
		default:
			http.NotFound(w, r)
		}
	}
}

// TestEnrichResults: each page's own text (tags stripped, entities decoded) is APPENDED after the
// hit's snippet — the snippet is Tavily's query-relevance extract and must survive — fetches run
// concurrently, and any failing fetch keeps the snippet alone.
func TestEnrichResults(t *testing.T) {
	allowLoopbackFetch(t)
	slow := make(chan struct{})
	page := httptest.NewServer(enrichTestPage(slow))
	defer page.Close()
	defer close(slow)

	results := []tavilyResult{
		{Title: "good", URL: page.URL + "/good", Content: "snippet g"},
		{Title: "binary", URL: page.URL + "/binary", Content: "snippet b"},
		{Title: "missing", URL: page.URL + "/404", Content: "snippet m"},
		{Title: "no-url", Content: "snippet n"},
	}
	enrichResults(context.Background(), results)
	if want := "snippet g\n\nReal & Deep page body text"; results[0].Content != want {
		t.Errorf("good page content = %q, want %q (snippet kept, page text appended)", results[0].Content, want)
	}
	for _, r := range results[1:] {
		if !strings.HasPrefix(r.Content, "snippet") {
			t.Errorf("%s: failed enrichment must keep the snippet, got %q", r.Title, r.Content)
		}
	}
	checkEnrichSlowBounded(t, page.URL+"/slow")
}

// checkEnrichSlowBounded: a hung fetch is bounded by ctx, not waited on serially, and keeps its snippet.
func checkEnrichSlowBounded(t *testing.T, slowURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	slowres := []tavilyResult{
		{URL: slowURL, Content: "snippet s1"},
		{URL: slowURL, Content: "snippet s2"},
	}
	enrichResults(ctx, slowres)
	if e := time.Since(start); e > 2*time.Second {
		t.Fatalf("two hung fetches took %v — they must share one bounded wait, not queue", e)
	}
	for _, r := range slowres {
		if !strings.HasPrefix(r.Content, "snippet") {
			t.Errorf("timed-out fetch must keep the snippet, got %q", r.Content)
		}
	}
}

func TestPingReturnsEmptyResult(t *testing.T) {
	srv := &Server{Search: func(context.Context, string, int) (string, error) { return "x", nil }}
	resps := drive(t, srv, `{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if len(resps) != 1 || resps[0]["error"] != nil || resps[0]["result"] == nil {
		t.Errorf("ping = %v, want an empty result and no error", resps)
	}
}

// allowLoopbackFetch swaps the SSRF-guarded pageClient for a plain one so httptest servers on 127.0.0.1 can be fetched.
func allowLoopbackFetch(t *testing.T) {
	t.Helper()
	orig := pageClient
	pageClient = &http.Client{}
	t.Cleanup(func() { pageClient = orig })
}

// TestFetchPageTextRefusesInternalAddresses: a loopback target and a public-looking redirect to one both keep the snippet.
func TestFetchPageTextRefusesInternalAddresses(t *testing.T) {
	var hits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "secret metadata")
	}))
	defer internal.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/x", http.StatusFound)
	}))
	defer redir.Close()
	for _, u := range []string{internal.URL + "/x", redir.URL + "/r"} {
		if got := fetchPageText(context.Background(), u); got != "" {
			t.Errorf("fetchPageText(%s) = %q, want refusal", u, got)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("internal server was reached %d times, want 0", n)
	}
}

// TestFetchRobotsTimesOutOnSilentHost: a host that accepts and never answers cannot block a deadline-less ctx.
func TestFetchRobotsTimesOutOnSilentHost(t *testing.T) {
	allowLoopbackFetch(t)
	orig := enrichTimeout
	enrichTimeout = 100 * time.Millisecond
	t.Cleanup(func() { enrichTimeout = orig })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	done := make(chan string, 1)
	go func() { done <- fetchRobots(context.Background(), srv.URL) }()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("fetchRobots = %q, want empty", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fetchRobots still blocked after 2s on a silent host")
	}
}

// TestFetchPageTextHonorsRobots: a Disallow for * keeps the snippet and the page is never requested.
func TestFetchPageTextHonorsRobots(t *testing.T) {
	allowLoopbackFetch(t)
	var pageHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
			return
		}
		pageHits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<p>body</p>")
	}))
	defer srv.Close()
	if got := fetchPageText(context.Background(), srv.URL+"/private/a"); got != "" || pageHits.Load() != 0 {
		t.Errorf("disallowed page: got %q, hits %d, want empty and 0", got, pageHits.Load())
	}
	if got := fetchPageText(context.Background(), srv.URL+"/open"); got != "body" {
		t.Errorf("allowed page = %q, want body", got)
	}
}

// TestFetchPageTextTruncatedBodyKeepsSnippet: a body cut off mid-read yields "" rather than a fragment.
func TestFetchPageTextTruncatedBodyKeepsSnippet(t *testing.T) {
	allowLoopbackFetch(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "1000")
		fmt.Fprint(w, "<p>partial")
	}))
	defer srv.Close()
	if got := fetchPageText(context.Background(), srv.URL+"/p"); got != "" {
		t.Errorf("truncated body gave %q, want empty", got)
	}
}

// TestRobotsAllowedSeveralUserAgentsShareGroup: consecutive User-agent lines form one group, so the Disallow binds "*".
func TestRobotsAllowedSeveralUserAgentsShareGroup(t *testing.T) {
	allowLoopbackFetch(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nUser-agent: Googlebot\nDisallow: /\n")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/page")
	if robotsAllowed(context.Background(), u, map[string]string{}) {
		t.Error("robotsAllowed = true, want false: Disallow / applies to the * group")
	}
}

// TestFetchPageTextRechecksRobotsOnRedirect: an allowed URL redirecting to a Disallowed path, or to another origin
// that disallows everything, is refused before the target page is requested.
func TestFetchPageTextRechecksRobotsOnRedirect(t *testing.T) {
	allowLoopbackFetch(t)
	var bHits, blockedHits atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
			return
		}
		bHits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<p>b</p>")
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			fmt.Fprint(w, "User-agent: *\nDisallow: /blocked\n")
		case "/start":
			http.Redirect(w, r, "/blocked", http.StatusFound)
		case "/cross":
			http.Redirect(w, r, b.URL+"/page", http.StatusFound)
		case "/hop":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			if r.URL.Path == "/blocked" {
				blockedHits.Add(1)
			}
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<p>a</p>")
		}
	}))
	defer a.Close()
	for _, p := range []string{"/start", "/cross"} {
		if got := fetchPageText(context.Background(), a.URL+p); got != "" {
			t.Errorf("fetchPageText(%s) = %q, want refusal", p, got)
		}
	}
	if bHits.Load() != 0 || blockedHits.Load() != 0 {
		t.Errorf("disallowed targets requested: b=%d blocked=%d, want 0", bHits.Load(), blockedHits.Load())
	}
	if got := fetchPageText(context.Background(), a.URL+"/hop"); got != "a" {
		t.Errorf("allowed redirect = %q, want a", got)
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

type notFoundRT struct{}

func (notFoundRT) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 404, Body: http.NoBody, Request: r}, nil
}

func TestExtractPrintsPagesAndFailures(t *testing.T) {
	t.Setenv(keyName, "test-key")
	origClient := pageClient
	pageClient = &http.Client{Transport: notFoundRT{}} // robots.txt 404 -> allowed, no network
	t.Cleanup(func() { pageClient = origClient })
	orig := tavilyExtract
	t.Cleanup(func() { tavilyExtract = orig })
	var calls []string
	tavilyExtract = func(_ context.Context, _ string, urls []string, depth string) (extractResponse, error) {
		calls = append(calls, depth+":"+strings.Join(urls, ","))
		var r extractResponse
		for _, u := range urls {
			if u == "https://example.com/a" || (u == "https://example.org/b" && depth == "advanced") {
				r.Results = append(r.Results, struct {
					URL        string `json:"url"`
					RawContent string `json:"raw_content"`
				}{u, "# Hello\nbody " + depth})
				continue
			}
			r.Failed = append(r.Failed, struct {
				URL   string `json:"url"`
				Error string `json:"error"`
			}{u, "Failed to fetch url"})
		}
		return r, nil
	}
	urls := []string{"https://example.com/a", "https://example.org/b", "https://example.net/c"}
	var code int
	out := captureStdout(t, func() { code = Extract(context.Background(), urls) })
	wantCalls := "basic:https://example.com/a,https://example.org/b,https://example.net/c|advanced:https://example.org/b,https://example.net/c"
	if code != 0 || strings.Join(calls, "|") != wantCalls {
		t.Fatalf("code=%d calls=%v, want 0 and only basic failures retried at advanced depth", code, calls)
	}
	for _, want := range []string{
		"# https://example.com/a\n# Hello\nbody basic",
		"# https://example.org/b\n# Hello\nbody advanced",
		"# https://example.net/c\nfailed: Failed to fetch url",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestExtractRefusesInternalAddresses(t *testing.T) {
	t.Setenv(keyName, "test-key")
	orig := tavilyExtract
	t.Cleanup(func() { tavilyExtract = orig })
	tavilyExtract = func(context.Context, string, []string, string) (extractResponse, error) {
		t.Error("seam called for an internal URL")
		return extractResponse{}, nil
	}
	var code int
	out := captureStdout(t, func() {
		code = Extract(context.Background(), []string{"http://127.0.0.1/", "http://192.168.1.1/", "file:///etc/passwd"})
	})
	if code != 1 || strings.Count(out, "failed:") != 3 || !strings.Contains(out, "refusing to fetch internal address 192.168.1.1") {
		t.Errorf("code=%d out=%q, want exit 1 with 3 refusals", code, out)
	}
}

func TestPublicIPRefusesCGNATAndZeroNet(t *testing.T) {
	for _, s := range []string{"100.100.100.100", "100.64.0.1", "0.1.2.3", "ff02::1", "ff0e::1"} {
		if publicIP(net.ParseIP(s)) {
			t.Errorf("publicIP(%s) = true, want false", s)
		}
	}
	if !publicIP(net.ParseIP("100.128.0.1")) || !publicIP(net.ParseIP("8.8.8.8")) {
		t.Error("public addresses outside CGNAT must stay allowed")
	}
}

func TestExtractMatchesTavilyNormalisedURL(t *testing.T) {
	t.Setenv(keyName, "test-key")
	origClient := pageClient
	pageClient = &http.Client{Transport: notFoundRT{}}
	t.Cleanup(func() { pageClient = origClient })
	orig := tavilyExtract
	t.Cleanup(func() { tavilyExtract = orig })
	tavilyExtract = func(_ context.Context, _ string, _ []string, _ string) (extractResponse, error) {
		var r extractResponse
		r.Results = append(r.Results, struct {
			URL        string `json:"url"`
			RawContent string `json:"raw_content"`
		}{"https://example.com/", "BODY"})
		r.Failed = append(r.Failed, struct {
			URL   string `json:"url"`
			Error string `json:"error"`
		}{"https://EXAMPLE.org", "boom"})
		return r, nil
	}
	var code int
	out := captureStdout(t, func() {
		code = Extract(context.Background(), []string{"https://example.com", "https://example.org"})
	})
	if code != 0 || !strings.Contains(out, "# https://example.com\nBODY") || !strings.Contains(out, "failed: boom") {
		t.Errorf("code=%d out=%q, want body for normalised echo and failure surfaced", code, out)
	}
}
