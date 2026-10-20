package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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

// TestEnrichResults: each page's own text (tags stripped, entities decoded) is APPENDED after the
// hit's snippet — the snippet is Tavily's query-relevance extract and must survive — fetches run
// concurrently, and any failing fetch keeps the snippet alone.
func TestEnrichResults(t *testing.T) {
	slow := make(chan struct{})
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
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

	// The slow fetch is bounded by ctx, not waited on serially: a short deadline returns promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	slowres := []tavilyResult{
		{URL: page.URL + "/slow", Content: "snippet s1"},
		{URL: page.URL + "/slow", Content: "snippet s2"},
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
