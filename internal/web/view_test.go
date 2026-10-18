package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agenttree"
)

// withEmptyDispatchDir points internal/agent's dispatch log at an empty temp dir for the duration of
// the test, so a /api/view request's vendor-leg lookup never touches this machine's real dispatch
// log under ~/.claude/state/harness/dispatch.
func withEmptyDispatchDir(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// --- /api/view guard (host allowlist) ---

func TestHandleViewGuardRejectsBadHost(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	req := newReq(t, "GET", "/api/view?session=finished-session-aaaa1111&agent=fakewfagent002", "evil.example")
	w := do(s, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMisdirectedRequest)
	}
}

func TestHandleViewGuardRejectsCrossOrigin(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	req := newReq(t, "GET", "/api/view?session=finished-session-aaaa1111&agent=fakewfagent002", "127.0.0.1:7777")
	req.Header.Set("Origin", "http://evil.example")
	w := do(s, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// --- missing/unknown agent: 400/404, never 500 ---

func TestHandleViewMissingAgentIsBadRequest(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	w := do(s, newReq(t, "GET", "/api/view?session=finished-session-aaaa1111", "127.0.0.1:7777"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing ?agent: status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleViewUnknownAgentIsNotFoundNot500(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	w := do(s, newReq(t, "GET", "/api/view?agent=no-such-agent-at-all", "127.0.0.1:7777"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: status = %d, want 404 (not 500); body=%s", w.Code, w.Body.String())
	}
}

// --- snapshot JSON decodes to agenttree.Snapshot ---

func TestHandleViewSnapshotDecodesAgenttreeSnapshot(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	req := newReq(t, "GET", "/api/view?session=finished-session-aaaa1111&agent=fakewfagent002", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var snap agenttree.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode /api/view body into agenttree.Snapshot: %v; body=%s", err, w.Body.String())
	}
	if snap.ID != "fakewfagent002" {
		t.Errorf("snap.ID = %q, want fakewfagent002", snap.ID)
	}
	if snap.SessionID != "finished-session-aaaa1111" {
		t.Errorf("snap.SessionID = %q, want finished-session-aaaa1111", snap.SessionID)
	}
}

// --- follow=1 streams at least one line for an agent that already has a terminal result ---

func TestHandleViewFollowStreamsResultLine(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	req := newReq(t, "GET", "/api/view?session=finished-session-aaaa1111&agent=fakewfagent002&follow=1", "127.0.0.1:7777")
	w := do(s, req)
	if w.Code != http.StatusOK {
		t.Fatalf("follow=1: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("follow=1 Content-Type = %q, want text/plain", ct)
	}
	// fakewfagent002's journal already carries a result (see internal/agenttree's own
	// TestViewSnapshotFields), so Follow emits that result line on its very first poll and returns.
	if lines := strings.TrimSpace(w.Body.String()); lines == "" {
		t.Fatal("follow=1 streamed zero lines for an already-resulted agent")
	} else if !strings.Contains(lines, "result") {
		t.Errorf("follow=1 body = %q, want a line containing %q", lines, "result")
	}
}

// --- follow=1 stops when the request context is cancelled, for an agent that never finishes ---

// TestHandleViewFollowStopsOnContextCancel uses an agent with no new events and no terminal result
// (agent-fakeagentid002.meta.json: "finished":false): the headers arrive at once (handleView flushes
// before the poll loop), the body stays open, and cancelling the request must end the server-side
// Follow loop and the body read promptly.
func TestHandleViewFollowStopsOnContextCancel(t *testing.T) {
	withEmptyDispatchDir(t)
	s := newTestServer(t, Options{ClaudeDir: agenttreeTestdata, Days: 36500}, nil)
	// A real server (not httptest.ResponseRecorder) so cancelling mid-request is a genuine client
	// disconnect the server side can observe via r.Context().
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET",
		srv.URL+"/api/view?session=finished-session-aaaa1111&agent=fakeagentid002&follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:7777"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() must return as soon as the headers are flushed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(resp.Body) // blocks until the stream ends
		errc <- err
	}()

	time.Sleep(200 * time.Millisecond) // let handleView reach agenttree.Follow's poll loop
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("the body ended cleanly although the request was cancelled before any event")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("body read error = %v, want it to wrap context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the request context did not unblock handleView's follow within 5s")
	}
}
