package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/tree"
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

// --- snapshot JSON decodes to tree.Snapshot ---

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
	var snap tree.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode /api/view body into tree.Snapshot: %v; body=%s", err, w.Body.String())
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

	time.Sleep(200 * time.Millisecond) // let handleView reach tree.Follow's poll loop
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

// --- follow=1 must survive the server's own WriteTimeout: handleView clears the write deadline,
// so a late event (after the timeout would otherwise have fired) still reaches the client ---

func TestFollowSurvivesWriteTimeout(t *testing.T) {
	withEmptyDispatchDir(t)
	claudeDir := t.TempDir()
	if err := copyDir(t, agenttreeTestdata, claudeDir); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(claudeDir, "projects", "-Users-fake-demo-repo",
		"finished-session-aaaa1111", "subagents", "agent-fakeagentid002.jsonl")

	s := newTestServer(t, Options{ClaudeDir: claudeDir, Days: 36500}, nil)
	ts := httptest.NewUnstartedServer(s.mux)
	ts.Config.WriteTimeout = 300 * time.Millisecond
	ts.Start()
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET",
		ts.URL+"/api/view?session=finished-session-aaaa1111&agent=fakeagentid002&follow=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:7777"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do(): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	go func() {
		time.Sleep(350 * time.Millisecond) // past ts.Config.WriteTimeout; Follow emits it at its 500ms tick
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintln(f, `{"type":"assistant","message":{"content":[{"type":"text","text":"after write timeout"}]}}`)
	}()

	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "after write timeout") {
			return
		}
	}
	t.Fatalf("stream ended without the post-timeout line; scanner err=%v", sc.Err())
}

// copyDir recursively copies src into dst (dst must already exist).
func copyDir(t *testing.T, src, dst string) error {
	t.Helper()
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	})
}
