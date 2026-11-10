package websearch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRobotsDisallowsLongestMatch(t *testing.T) {
	const txt = `# comment
User-agent: *
Disallow: /star-private
Disallow: /

User-agent: claude-code
Disallow: /private
Disallow: /*.json$
Disallow: /search?q=
Allow: /private/public
Disallow: /tie
Allow: /tie
Disallow: /fix$
`
	cases := []struct {
		path    string
		blocked bool
		rule    string
	}{
		{"/", false, ""},                       // claude-code group beats the blanket * Disallow: /
		{"/star-private", false, ""},           // * group rules are ignored entirely
		{"/private", true, "/private"},         // prefix
		{"/private/x", true, "/private"},       // prefix covers children
		{"/private/public/a", false, ""},       // longer Allow beats shorter Disallow
		{"/data/x.json", true, "/*.json$"},     // wildcard + anchor
		{"/data/x.json?a=1", false, ""},        // $ anchors the end, query defeats it
		{"/data/x.jsonl", false, ""},           // $ again
		{"/search?q=cats", true, "/search?q="}, // query is part of the match path
		{"/tie", false, ""},                    // equal length: Allow wins
		{"/fix", true, "/fix$"},                // exact via $
		{"/fixed", false, ""},                  // $ blocks prefix-extension
		{"/other", false, ""},
	}
	rules := parseRobots(txt)
	for _, tc := range cases {
		r, blocked := robotsDisallows(rules, tc.path)
		if blocked != tc.blocked || (blocked && r.pattern != tc.rule) {
			t.Errorf("%s: blocked=%v rule=%q, want %v %q", tc.path, blocked, r.pattern, tc.blocked, tc.rule)
		}
	}
}

func TestParseRobotsEmptyDisallowIsAllowAllGroup(t *testing.T) {
	cases := []struct {
		name, txt string
		blocked   bool
	}{
		{"claude-code allow-all beats * Disallow /", "User-agent: claude-code\nDisallow:\n\nUser-agent: *\nDisallow: /\n",
			false},
		{"claude-code Disallow /x applies", "User-agent: claude-code\nDisallow: /x\n\nUser-agent: *\nDisallow:\n", true},
		{"only * Disallow / applies", "User-agent: *\nDisallow: /\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, blocked := robotsDisallows(parseRobots(c.txt), "/x/page")
			if blocked != c.blocked {
				t.Errorf("blocked = %v, want %v", blocked, c.blocked)
			}
		})
	}
}

func TestRobotsMatchMultiWildcardIsBounded(t *testing.T) {
	pat := "/" + strings.Repeat("*a", 10) + "*b"
	path := "/" + strings.Repeat("a", 60)
	done := make(chan bool, 1)
	go func() { done <- robotsMatch(pat, path) }()
	select {
	case got := <-done:
		if got {
			t.Errorf("robotsMatch(%q) matched a path without b", pat)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("robotsMatch did not finish: exponential backtracking")
	}
	if !robotsMatch(pat, path+"b") {
		t.Errorf("robotsMatch(%q) must match a path ending in b", pat)
	}
	if robotsMatch("/a*b$", "/aXbY") || !robotsMatch("/a*b$", "/aXb") {
		t.Errorf("trailing $ must anchor the last segment")
	}
}

func TestParseRobotsStripsLeadingBOM(t *testing.T) {
	_, blocked := robotsDisallows(parseRobots("\ufeffUser-agent: *\nDisallow: /\n"), "/x/page")
	if !blocked {
		t.Error("BOM-prefixed first group was ignored; want Disallow / to apply")
	}
}

// resetRobotsCache empties the package robots.txt cache for this test and again after it.
func resetRobotsCache(t *testing.T) {
	t.Helper()
	clearCache := func() {
		robotsCache.Lock()
		clear(robotsCache.m)
		robotsCache.Unlock()
	}
	clearCache()
	origTTL := robotsTTL
	t.Cleanup(func() { clearCache(); robotsTTL = origTTL })
}

// TestRobotsCacheHitWithinTTL: two pages on one origin fetch robots.txt once; past the TTL it is fetched again.
func TestRobotsCacheHitWithinTTL(t *testing.T) {
	allowLoopbackFetch(t)
	var robotsHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsHits.Add(1)
			fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<p>ok</p>")
	}))
	defer srv.Close()
	ctx := context.Background()
	for _, p := range []string{"/a", "/b", "/private/x"} {
		fetchPageText(ctx, srv.URL+p)
	}
	if n := robotsHits.Load(); n != 1 {
		t.Fatalf("robots.txt fetched %d times within the TTL, want 1", n)
	}
	robotsTTL = 0
	if u, _ := url.Parse(srv.URL + "/private/y"); robotsAllowed(ctx, u) || robotsHits.Load() != 2 {
		t.Errorf("past TTL: robots hits = %d, want a refetch that still disallows /private", robotsHits.Load())
	}
}

// TestPageClientTransportBounds: no proxy (the dial guard must see the real target), every phase bounded.
func TestPageClientTransportBounds(t *testing.T) {
	tr, ok := pageClient.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || tr.DialContext == nil {
		t.Fatalf("pageClient transport = %#v, want a direct dial-guarded *http.Transport", pageClient.Transport)
	}
	for name, d := range map[string]time.Duration{"client": pageClient.Timeout, "tls": tr.TLSHandshakeTimeout,
		"header": tr.ResponseHeaderTimeout} {
		if d < enrichTimeout {
			t.Errorf("%s timeout %v undercuts the %v per-fetch ctx bound", name, d, enrichTimeout)
		}
	}
	if tr.IdleConnTimeout == 0 || tr.MaxIdleConnsPerHost < 2 {
		t.Errorf("idle pool: timeout %v, per-host %d; want bounded keep-alives", tr.IdleConnTimeout, tr.MaxIdleConnsPerHost)
	}
}
