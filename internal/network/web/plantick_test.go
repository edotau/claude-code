package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tickReq(t *testing.T, token string, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/plan/tick", strings.NewReader(string(b)))
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(csrfHeader, token)
	}
	return req
}

// Spanning: the endpoint ticks with s.now(), then answers 403/409/400 and leaves the plan alone on each.
func TestPlanTickEndpoint(t *testing.T) {
	s, dir := plansServer(t)
	fixed := time.Date(2026, 10, 31, 12, 30, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	rel := "2026-10-01-old.md"
	full := filepath.Join(dir, "plans", rel)
	title := "1.2 Open owns: b.go"
	body := func(path, ttl string) map[string]any {
		return map[string]any{"path": path, "line": 6, "title": ttl, "done": true}
	}
	read := func() string { b, _ := os.ReadFile(full); return string(b) }
	orig := read()

	if w := do(s, tickReq(t, "", body(rel, title))); w.Code != http.StatusForbidden {
		t.Fatalf("no CSRF = %d, want 403", w.Code)
	}
	if w := do(s, tickReq(t, testCSRFToken, body(rel, "9.9 Nope"))); w.Code != http.StatusConflict {
		t.Fatalf("wrong title = %d %s, want 409", w.Code, w.Body)
	}
	for _, p := range []string{"../settings.md", "../../etc/passwd"} {
		if w := do(s, tickReq(t, testCSRFToken, body(p, title))); w.Code != http.StatusBadRequest {
			t.Fatalf("path %q = %d, want 400", p, w.Code)
		}
	}
	if read() != orig {
		t.Fatalf("rejected requests changed the plan:\n%s", read())
	}

	w := do(s, tickReq(t, testCSRFToken, body(rel, title)))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("tick = %d %s", w.Code, w.Body)
	}
	got := read()
	if !strings.Contains(got, "- [x] 1.2 Open") || !strings.Contains(got, "<!-- user: ticked 2026-10-31T12:30:00Z -->") ||
		!strings.Contains(got, "<!-- working: bob -->") {
		t.Fatalf("plan after tick:\n%s", got)
	}
}
