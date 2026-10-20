package web

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/browser"
)

// countingBrowserEnv returns a browser.Env whose Run never touches a real binary (every probe
// reports "not found") and counts how many times it was invoked, so cache-hit vs. cache-miss
// behavior can be asserted without mdfind/xdg-settings/defaults ever running for real.
func countingBrowserEnv(t *testing.T) (browser.Env, *int) {
	t.Helper()
	n := 0
	var mu sync.Mutex
	return browser.Env{
		Home:       t.TempDir(),
		AppSupport: t.TempDir(),
		GOOS:       "linux", // deterministic probe set off this machine's actual installed browsers
		Run: func(name string, args ...string) (string, error) {
			mu.Lock()
			n++
			mu.Unlock()
			return "", errNotFound
		},
		Now: time.Now,
	}, &n
}

type notFoundErr string

func (e notFoundErr) Error() string { return string(e) }

const errNotFound = notFoundErr("not found")

// newBrowserTestServer is newTestServer plus an injected fake browser.Env and clock, mirroring
// newStatusTestServer's pattern for /api/dispatches.
func newBrowserTestServer(t *testing.T, env browser.Env, now func() time.Time) *Server {
	t.Helper()
	s := newTestServer(t, Options{}, nil)
	s.browserEnv = env
	s.browsers = newBrowserCache()
	s.now = now
	return s
}

// --- /api/browsers guard (host allowlist) ---

func TestHandleBrowsersGuardRejectsBadHost(t *testing.T) {
	env, _ := countingBrowserEnv(t)
	s := newBrowserTestServer(t, env, time.Now)
	req := newReq(t, "GET", "/api/browsers", "evil.example")
	w := do(s, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMisdirectedRequest)
	}
}

func TestHandleBrowsersGuardRejectsCrossOrigin(t *testing.T) {
	env, _ := countingBrowserEnv(t)
	s := newBrowserTestServer(t, env, time.Now)
	req := newReq(t, "GET", "/api/browsers", "127.0.0.1:7777")
	req.Header.Set("Origin", "http://evil.example")
	w := do(s, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// --- decodes to browser.Report ---

func TestHandleBrowsersDecodesReport(t *testing.T) {
	env, _ := countingBrowserEnv(t)
	s := newBrowserTestServer(t, env, time.Now)
	w := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var rep browser.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode /api/browsers body into browser.Report: %v; body=%s", err, w.Body.String())
	}
	if len(rep.Browsers) == 0 {
		t.Error("Report.Browsers is empty, want every known browser reported (even if not-installed)")
	}
}

// --- cached within the TTL: one browser.Detect probe pass across three requests ---

func TestHandleBrowsersServesWithinTTLFromCacheWithoutReDetecting(t *testing.T) {
	env, calls := countingBrowserEnv(t)
	fakeNow := time.Now()
	s := newBrowserTestServer(t, env, func() time.Time { return fakeNow })

	for i := range 3 {
		w := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, w.Code)
		}
	}
	first := *calls
	if first == 0 {
		t.Fatal("expected at least one Env.Run call from the first request's detection pass")
	}
	// A second or third request within the cache TTL must not trigger any further Env.Run calls:
	// the call count after 3 requests must equal the count after request 1.
	w := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w.Code != http.StatusOK {
		t.Fatalf("4th request: status = %d, want 200", w.Code)
	}
	if *calls != first {
		t.Errorf("4 requests within the cache TTL triggered more Env.Run calls: first-pass=%d, after=%d", first, *calls)
	}
}

// --- re-detects once the TTL has elapsed ---

func TestHandleBrowsersRedetectsAfterTTL(t *testing.T) {
	env, calls := countingBrowserEnv(t)
	fakeNow := time.Now()
	s := newBrowserTestServer(t, env, func() time.Time { return fakeNow })

	w1 := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", w1.Code)
	}
	first := *calls

	// Still within the TTL: no further probing.
	w2 := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w2.Code != http.StatusOK {
		t.Fatalf("second request: status = %d, want 200", w2.Code)
	}
	if *calls != first {
		t.Fatalf("precondition failed: second request within TTL re-probed (calls %d -> %d)", first, *calls)
	}

	// Advance the fake clock past the cache TTL: the next request must trigger a fresh detection pass.
	fakeNow = fakeNow.Add(browserCacheTTL + time.Second)
	w3 := do(s, newReq(t, "GET", "/api/browsers", "127.0.0.1:7777"))
	if w3.Code != http.StatusOK {
		t.Fatalf("third request (post-expiry): status = %d, want 200", w3.Code)
	}
	if *calls <= first {
		t.Errorf("a request past the cache TTL should re-detect, got %d calls after expiry, want > %d", *calls, first)
	}
}
