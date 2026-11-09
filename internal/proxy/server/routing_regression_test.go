package server

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestScopedAuthDeadProviderReturnsForbidden(t *testing.T) {
	gate := useGate(t)
	up1 := newUpstream(t, ok)
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	var routed *Router
	srv := setupRouter(t, failoverOverlay(up1.URL, up2.URL), func(r *Router) { routed = r })
	token, err := routed.sessions.mint("up1", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	gate.premark("up1") // attempts drops up1; session confinement then removes up2.

	resp := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-opus"}`, map[string]string{"X-Api-Key": token})
	got := body(t, resp)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(got, "no provider in this session's scope is usable") {
		t.Fatalf("auth-dead scoped request = %d %s, want 403 without a panic", resp.StatusCode, got)
	}
	if up1.hits.Load() != 0 || up2.hits.Load() != 0 {
		t.Fatalf("scoped request escaped to upstreams: up1=%d up2=%d", up1.hits.Load(), up2.hits.Load())
	}
}

// stalledUploadUpstream accepts headers but never reads the body. A small receive
// buffer keeps the large request in the write phase, before ResponseHeaderTimeout.
func stalledUploadUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	up.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateNew {
			if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
				t.Errorf("set stalled upstream receive buffer: %v", err)
			}
		}
	}
	up.Start()
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(release) }) // unblock handlers before httptest waits for them.
	return up
}

func TestStalledLargeUploadFailsOverWithinAttemptBound(t *testing.T) {
	previous := uploadTimeout
	uploadTimeout = 2 * time.Second // covers the healthy leg too: proxy and upstream each parse the 16 MiB body
	t.Cleanup(func() { uploadTimeout = previous })
	stalled := stalledUploadUpstream(t)
	healthy := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	var routed *Router
	setupRouter(t, failoverOverlay(stalled.URL, healthy.URL), func(r *Router) { routed = r })

	payload := `{"model":"m1-opus","messages":[{"role":"user","content":"` + strings.Repeat("x", 16<<20) + `"}]}`
	req := testRequest(t, http.MethodPost, "http://127.0.0.1/p/up1/v1/messages", strings.NewReader(payload))
	req.Header.Set("X-Api-Key", secret)
	var interruptedWrite atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err != nil {
				interruptedWrite.Store(true)
			}
		},
	}))
	reply := httptest.NewRecorder()
	routed.ServeHTTP(reply, req)
	if healthy.hits.Load() != 1 || reply.Code != http.StatusOK || !strings.Contains(reply.Body.String(), `"type":"message"`) {
		t.Fatalf("stalled upload did not fail over within attempt bound: healthy hits=%d status=%d body=%s", healthy.hits.Load(), reply.Code, reply.Body.String())
	}
	if !interruptedWrite.Load() {
		t.Fatal("upload completed before cancellation; regression did not exercise a stalled write")
	}
}

func sessionSnapshot(t *testing.T, g *sessionRig, token string) session {
	t.Helper()
	g.r.sessions.mu.Lock()
	defer g.r.sessions.mu.Unlock()
	ses := g.r.sessions.m[sessionHash(token)]
	if ses == nil {
		t.Fatal("session unexpectedly removed")
	}
	return *ses
}

func TestFailedIdentityProbeRejectsWithoutRenewingTrust(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	g := newSessionRig(t)
	token := g.mint(t, "dev")
	before := sessionSnapshot(t, g, token)
	var failed atomic.Bool
	failed.Store(true)
	g.r.sessions.procStart = func(int) (string, error) {
		if failed.Load() {
			return "", errors.New("process identity temporarily unavailable")
		}
		return "100", nil
	}
	g.advance(sessionVerify)
	if code, _ := g.call(t, "/p/dev/v1/messages", `{}`, token); code != http.StatusUnauthorized {
		t.Errorf("failed identity probe accepted request: status=%d, want 401", code)
	}
	if after := sessionSnapshot(t, g, token); after != before {
		t.Errorf("failed identity probe renewed session: before=%+v after=%+v", before, after)
	}
	g.advance(time.Second) // inside a wrongly renewed trust window: failure must still reject.
	if code, _ := g.call(t, "/p/dev/v1/messages", `{}`, token); code != http.StatusUnauthorized {
		t.Errorf("failed identity probe gained cached trust: status=%d, want 401", code)
	}
	failed.Store(false)
	if code, _ := g.call(t, "/p/dev/v1/messages", `{}`, token); code == http.StatusUnauthorized {
		t.Fatal("session did not recover after a verified identity probe")
	}
	after := sessionSnapshot(t, g, token)
	elapsed := sessionVerify + time.Second
	if !after.checked.Equal(before.checked.Add(elapsed)) || !after.expires.Equal(before.expires.Add(elapsed)) {
		t.Fatalf("successful probe did not renew trust and expiry: before=%+v after=%+v", before, after)
	}
}
