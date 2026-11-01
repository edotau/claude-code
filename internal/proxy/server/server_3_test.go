package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestChatPathPromptTooLong checks the classified limit text from chat translation.
func TestChatPathPromptTooLong(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"prompt is too long: 1177204 tokens > 1000000 maximum","secret":"leak-me"}}`)
	})
	t.Setenv("OA_KEY", "oa")
	srv := setup(t, `{"default":"oa","providers":{"openai":{"oa":{"protocol":"openai","base_url":"`+up.URL+`/v1","auth":{"type":"key","env":"OA_KEY"},"models":{"opus":"gpt-x"}}}}}`)
	resp := post(t, srv.URL+"/p/oa/v1/messages", `{"model":"gpt-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, nil)
	b, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct{ Type, Message string }
	}
	_ = json.Unmarshal(b, &e)
	if resp.StatusCode != 400 || e.Error.Type != "invalid_request_error" ||
		e.Error.Message != "prompt is too long: 1177204 tokens > 1000000 maximum" || strings.Contains(string(b), "leak-me") {
		t.Fatalf("status %d body %s", resp.StatusCode, b)
	}
}

// Regression: a side proxy (--serve --port N) must neither overwrite nor delete a live daemon's router.json.
func TestServeSideProxyKeepsDaemonState(t *testing.T) {
	vaulttest.Use(t)
	daemon := proxy.State{PID: os.Getppid(), Port: 1, Started: time.Now().UTC()} // a live pid on another port
	data, _ := json.Marshal(daemon)
	if err := paths.AtomicWrite(filepath.Join(paths.StateDir(), "router.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	listening := make(chan struct{})
	orig := serveListening
	serveListening = func() { close(listening) }
	t.Cleanup(func() { serveListening = orig })
	go func() { done <- Serve(ctx, 0) }()
	select {
	case <-listening:
	case err := <-done:
		t.Fatalf("Serve exited before listening: %v", err)
	}
	if cur, err := proxy.ReadState(); err != nil || cur.PID != daemon.PID || cur.Port != daemon.Port {
		t.Fatalf("while serving, router.json = %+v, %v; want the daemon's %+v", cur, err, daemon)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cur, err := proxy.ReadState(); err != nil || cur.PID != daemon.PID {
		t.Fatalf("after the side proxy exits, router.json = %+v, %v; want the daemon's kept", cur, err)
	}
}

func TestSessionTokenCannotReachOtherProfiles(t *testing.T) {
	up1 := newUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(429) })
	up2 := newUpstream(t, ok)
	t.Setenv("UP1_KEY", "k1")
	t.Setenv("UP2_KEY", "k2")
	srv := setup(t, failoverOverlay(up1.URL, up2.URL))
	req := testRequest(t, http.MethodPost, srv.URL+proxy.SessionPath, strings.NewReader(`{"profile":"up1","pid":`+strconv.Itoa(os.Getpid())+`}`))
	req.Header.Set("X-Api-Key", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out proxy.SessionResponse
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.Token == "" {
		t.Fatalf("mint = %d", resp.StatusCode)
	}
	hdr := map[string]string{"X-Api-Key": out.Token}
	if r := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"up2:x"}`, hdr); r.StatusCode != http.StatusForbidden {
		t.Errorf("cross-provider model = %d, want 403", r.StatusCode)
	}
	if r := post(t, srv.URL+"/p/up1/v1/messages", `{"model":"m1-haiku"}`, hdr); r.StatusCode == 200 {
		t.Errorf("failover escaped to up2 under a session token")
	}
	if up2.hits.Load() != 0 {
		t.Errorf("up2 hits = %d, want 0", up2.hits.Load())
	}
}
