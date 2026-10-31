package web

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/usage"
)

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
