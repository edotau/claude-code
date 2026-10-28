package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuf is a log sink the handler goroutine writes after the reply is already on the wire.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// A passthrough 429 logs the model, the real attempt count, the upstream request id, Retry-After and the
// sanitized error code, and the client still gets the upstream body unchanged.
func TestPassthroughLogsDiagnostics(t *testing.T) {
	const upBody = `{"error_code":"REQUEST_LIMIT_EXCEEDED","message":"slow down"}`
	up := newRawUp(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("X-Request-Id", "abc-123\nforged=1")
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, upBody)
	})
	t.Setenv("PT_KEY", "prov-cred")
	logs := &lockedBuf{}
	srv := setupRouter(t, ptOverlay(up.URL), func(r *Router) { r.log = logs })

	resp := ptDo(t, http.MethodPost, srv.URL+"/p/pt/responses/responses", `{"model":"databricks-gpt-6-1-sol","input":"hi"}`, nil)
	if b := body(t, resp); resp.StatusCode != http.StatusTooManyRequests || b != upBody {
		t.Fatalf("status %d body %q, want the upstream 429 verbatim", resp.StatusCode, b)
	}
	want := []string{" pt databricks-gpt-6-1-sol 429 1 ", " req=abc-123forged1", " retry_after=30", " err=REQUEST_LIMIT_EXCEEDED"}
	deadline := time.Now().Add(2 * time.Second) // the log line is written after the reply
	for {
		line := logs.String()
		missing := ""
		for _, w := range want {
			if !strings.Contains(line, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			if strings.Count(line, "\n") != 1 {
				t.Fatalf("log = %q, want exactly one line (a header must not inject a second)", line)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log line %q lacks %q", line, missing)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
