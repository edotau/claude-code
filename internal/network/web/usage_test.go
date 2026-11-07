package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/usage"
)

// The embedded copies must be byte-identical to the extension's modules; fix drift with `go generate ./internal/network/web`.
func TestUsageAssetsMatchExtensionSource(t *testing.T) {
	for _, name := range []string{"format", "pricing", "usage-model", "usage-view"} {
		src, err := os.ReadFile(filepath.Join("..", "..", "..", "vscode", "claude-code-agents", "lib", name+".js"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := assetsFS.ReadFile("assets/usage/" + name + ".js")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(src, got) {
			t.Errorf("assets/usage/%s.js differs from vscode/claude-code-agents/lib/%s.js; run go generate", name, name)
		}
	}
}

// assertUsageJSON requires a 200 whose body decodes to the same JSON tree as want.
func assertUsageJSON(t *testing.T, call int, w *httptest.ResponseRecorder, want []byte) {
	t.Helper()
	var got, wantTree any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantTree); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || !reflect.DeepEqual(got, wantTree) {
		t.Fatalf("call %d: %d %q, want 200 %q", call, w.Code, w.Body.String(), want)
	}
}

// /api/usage returns the masked JSON of usage.Report for the asked window, cached for usageTTL.
func TestUsageEndpointServesReportJSONAndCaches(t *testing.T) {
	calls, now := 0, time.Now()
	orig := usageReport
	usageReport = func(_, _ string, days int, _ bool, _ io.Writer) (*usage.Document, error) {
		calls++
		return &usage.Document{WindowDays: days}, nil
	}
	t.Cleanup(func() { usageReport = orig })
	s := newTestServer(t, Options{}, nil)
	s.now = func() time.Time { return now }

	var want bytes.Buffer
	if err := helptext.WriteJSON(&want, &usage.Document{WindowDays: 7}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		w := do(s, newReq(t, "GET", "/api/usage?days=7", "127.0.0.1:7777"))
		assertUsageJSON(t, i, w, want.Bytes())
	}
	if calls != 1 {
		t.Fatalf("report built %d times for two requests inside the TTL, want 1", calls)
	}
	now = now.Add(usageTTL + time.Second)
	do(s, newReq(t, "GET", "/api/usage?days=7", "127.0.0.1:7777"))
	if calls != 2 {
		t.Fatalf("report built %d times after the TTL, want 2", calls)
	}
	for target, host := range map[string]string{"/api/usage?days=0": "127.0.0.1:7777", "/api/usage": "evil.example"} {
		if w := do(s, newReq(t, "GET", target, host)); w.Code == http.StatusOK {
			t.Errorf("GET %s (Host %s) = 200, want a refusal", target, host)
		}
	}
}

// quotas=0 builds without quotas and never satisfies a full request for the same window.
func TestUsageQuotasOffHasItsOwnCacheEntry(t *testing.T) {
	var gotQuotas []bool
	orig := usageReport
	usageReport = func(_, _ string, days int, quotas bool, _ io.Writer) (*usage.Document, error) {
		gotQuotas = append(gotQuotas, quotas)
		return &usage.Document{WindowDays: days}, nil
	}
	t.Cleanup(func() { usageReport = orig })
	s := newTestServer(t, Options{}, nil)
	now := time.Now()
	s.now = func() time.Time { return now }

	for i, c := range []struct {
		target string
		want   []bool
	}{
		{"/api/usage?days=7&quotas=0", []bool{false}},
		{"/api/usage?days=7", []bool{false, true}},
		{"/api/usage?days=7&quotas=0", []bool{false, true}},
		{"/api/usage?days=7", []bool{false, true}},
	} {
		if w := do(s, newReq(t, "GET", c.target, "127.0.0.1:7777")); w.Code != http.StatusOK {
			t.Fatalf("step %d GET %s = %d", i, c.target, w.Code)
		}
		if !reflect.DeepEqual(gotQuotas, c.want) {
			t.Fatalf("step %d GET %s: builds withQuotas = %v, want %v", i, c.target, gotQuotas, c.want)
		}
	}
}

// The served body masks emails and token-shaped strings, and the cache holds the masked bytes.
func TestUsageEndpointMasksEmailAndTokens(t *testing.T) {
	const email, token = "eric@gmail.com", "sk-ant-oat01-abcdefghijklmnop"
	var doc usage.Document
	fixture := `{"providers":[{"provider":"claude","account":"` + email + `","note":"token ` + token + ` expired"}],"window_days":7}`
	if err := json.Unmarshal([]byte(fixture), &doc); err != nil {
		t.Fatal(err)
	}
	orig := usageReport
	usageReport = func(_, _ string, _ int, _ bool, _ io.Writer) (*usage.Document, error) { return &doc, nil }
	t.Cleanup(func() { usageReport = orig })
	s := newTestServer(t, Options{}, nil)

	for i := range 2 { // second call is served from the cache
		body := do(s, newReq(t, "GET", "/api/usage?days=7", "127.0.0.1:7777")).Body.String()
		if strings.Contains(body, email) || strings.Contains(body, token) || !strings.Contains(body, "e…@gmail.com") {
			t.Fatalf("call %d body not masked: %s", i, body)
		}
	}
}

func stubUsageReport(t *testing.T) {
	t.Helper()
	orig := usageReport
	usageReport = func(_, _ string, days int, _ bool, _ io.Writer) (*usage.Document, error) {
		return &usage.Document{WindowDays: days}, nil
	}
	t.Cleanup(func() { usageReport = orig })
}

// Both usage routes sit behind the Host/Origin guard; a rebinding Host or cross-site Origin never reaches the handler.
func TestUsageRoutesEnforceGuard(t *testing.T) {
	stubUsageReport(t)
	s := newTestServer(t, Options{}, nil)
	for _, target := range []string{"/api/usage", "/assets/usage/usage-model.js"} {
		if w := do(s, newReq(t, "GET", target, "evil.example")); w.Code != http.StatusMisdirectedRequest {
			t.Errorf("GET %s with a foreign Host = %d, want 421", target, w.Code)
		}
		req := newReq(t, "GET", target, "127.0.0.1:7777")
		req.Header.Set("Origin", "http://evil.example")
		if w := do(s, req); w.Code != http.StatusForbidden {
			t.Errorf("GET %s with a foreign Origin = %d, want 403", target, w.Code)
		}
		if w := do(s, newReq(t, "GET", target, "127.0.0.1:7777")); w.Code != http.StatusOK {
			t.Errorf("GET %s from the bound host = %d, want 200", target, w.Code)
		}
	}
}

// Only GET is routed; the response types and the days / asset-name bounds are fixed.
func TestUsageRouteShapes(t *testing.T) {
	stubUsageReport(t)
	s := newTestServer(t, Options{}, nil)
	for _, target := range []string{"/api/usage", "/assets/usage/usage-view.js"} {
		if w := do(s, newReq(t, "POST", target, "127.0.0.1:7777")); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", target, w.Code)
		}
	}
	w := do(s, newReq(t, "GET", "/api/usage", "127.0.0.1:7777"))
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("/api/usage Content-Type = %q, want application/json", ct)
	}
	w = do(s, newReq(t, "GET", "/assets/usage/usage-model.js", "127.0.0.1:7777"))
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("asset Content-Type = %q, want text/javascript", ct)
	}
	for target, want := range map[string]int{
		"/api/usage?days=365":           http.StatusOK,
		"/api/usage?days=366":           http.StatusBadRequest,
		"/api/usage?days=abc":           http.StatusBadRequest,
		"/assets/usage/nope.js":         http.StatusNotFound,
		"/assets/usage/..%2Findex.html": http.StatusNotFound,
	} {
		if w := do(s, newReq(t, "GET", target, "127.0.0.1:7777")); w.Code != want {
			t.Errorf("GET %s = %d, want %d", target, w.Code, want)
		}
	}
}

// A credential file and a transcript line carrying a token-shaped string must not surface in the real report's JSON.
func TestUsageEndpointOmitsSecretsFromConfigDir(t *testing.T) {
	const secret = "sk-ant-oat01-SECRETSECRETSECRET"
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"s1","timestamp":"2099-01-01T00:00:00Z","message":{"model":"claude-x","usage":{"input_tokens":5,"output_tokens":7},"content":"` + secret + `"}}` + "\n"
	for p, body := range map[string]string{
		filepath.Join(dir, ".credentials.json"): `{"claudeAiOauth":{"accessToken":"` + secret + `"}}`,
		filepath.Join(proj, "s1.jsonl"):         line,
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orig := usageReport
	usageReport = func(d, sess string, days int, _ bool, errw io.Writer) (*usage.Document, error) {
		return usage.Report(d, sess, days, false, errw) // quotas off: no network in a unit test
	}
	t.Cleanup(func() { usageReport = orig })
	s := newTestServer(t, Options{ClaudeDir: dir}, nil)
	w := do(s, newReq(t, "GET", "/api/usage?days=7", "127.0.0.1:7777"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"window_days": 7`) && !strings.Contains(w.Body.String(), `"window_days":7`) {
		t.Fatalf("status %d body %q, want 200 with window_days 7", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Errorf("response leaks the token-shaped string: %s", w.Body.String())
	}
}

// A slow build for one key must not block another key, and same-key callers share one build.
func TestUsageCacheBuildsOutsideLockAndCoalesces(t *testing.T) {
	var c usageCache
	now := func() time.Time { return time.Now() }
	release, started := make(chan struct{}), make(chan struct{})
	var builds atomic.Int32
	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			_, _ = c.get(usageKey{days: 30}, false, now, func() ([]byte, error) {
				if builds.Add(1) == 1 {
					close(started)
				}
				<-release
				return []byte("slow"), nil
			})
			done <- struct{}{}
		}()
	}
	<-started
	body, err := c.get(usageKey{days: 1}, false, now, func() ([]byte, error) { return []byte("fast"), nil })
	if err != nil || string(body) != "fast" {
		t.Fatalf("other key = %q, %v; want it to build while days=30 is in flight", body, err)
	}
	time.Sleep(50 * time.Millisecond) // let the second caller reach the in-flight build
	close(release)
	<-done
	<-done
	if n := builds.Load(); n != 1 {
		t.Fatalf("days=30 built %d times for two concurrent callers, want 1", n)
	}
}

// A build that panics must release its flight, or every later request for the key hangs forever.
func TestUsageCachePanickingBuildReleasesFlight(t *testing.T) {
	var c usageCache
	now := func() time.Time { return time.Now() }
	key := usageKey{days: 7}
	func() {
		defer func() { _ = recover() }()
		_, _ = c.get(key, false, now, func() ([]byte, error) { panic("boom") })
	}()
	got := make(chan string, 1)
	go func() {
		body, err := c.get(key, false, now, func() ([]byte, error) { return []byte("ok"), nil })
		got <- string(body) + fmt.Sprint(err)
	}()
	select {
	case v := <-got:
		if v != "ok<nil>" {
			t.Fatalf("second get = %q, want ok<nil>", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second get hung: the panicked build left its flight registered")
	}
}
