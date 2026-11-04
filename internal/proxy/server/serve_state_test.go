package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestServeCanceledContextDoesNotPublishState(t *testing.T) {
	vaulttest.Use(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Serve(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve with a canceled context = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "router.json")); !os.IsNotExist(err) {
		t.Fatalf("router.json must not be published for a canceled context: %v", err)
	}
}

func TestServeStateAndEnsure(t *testing.T) {
	vaulttest.Use(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, 0) }()
	st := waitRouterHealthy(t)
	base, err := proxy.Ensure(context.Background())
	if err != nil || base != proxy.Base(st.Port) {
		t.Fatalf("proxy.Ensure = %q, %v", base, err)
	}
	checkVaultSealedSecret(t)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "router.json")); !os.IsNotExist(err) {
		t.Fatalf("router.json not removed: %v", err)
	}
}

// waitRouterHealthy polls until this process's router answers health and returns its state.
func waitRouterHealthy(t *testing.T) proxy.State {
	t.Helper()
	var st proxy.State
	for range 100 {
		if s, ok := proxy.Running(context.Background()); ok {
			st = s
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st.Port == 0 || st.PID != os.Getpid() {
		t.Fatalf("router never became healthy: %+v", st)
	}
	return st
}

// checkVaultSealedSecret asserts the client secret is stable, 64 chars and kept in the vault, not a file.
func checkVaultSealedSecret(t *testing.T) {
	t.Helper()
	s1, _ := proxy.ClientSecret()
	s2, _ := proxy.ClientSecret()
	_, err := os.Stat(filepath.Join(paths.StateDir(), "router.secret"))
	if !os.IsNotExist(err) || s1 == "" || s1 != s2 || len(s1) != 64 {
		t.Fatalf("secret %q/%q (vault-sealed, no file) stat err %v", s1, s2, err)
	}
}

// A stream past the drain is closed and Serve still returns nil: exiting 1 there reported every busy stop as a failure.
func TestServeClosesStreamsPastTheDrain(t *testing.T) {
	old := proxy.ShutdownDrain
	proxy.ShutdownDrain = 100 * time.Millisecond
	t.Cleanup(func() { proxy.ShutdownDrain = old })
	release := make(chan struct{})
	defer close(release)
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: ping\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	vaulttest.Use(t)
	overlay := fmt.Sprintf(`{"default":"up1","providers":{"anthropic":{"up1":{"protocol":"anthropic","base_url":%q,"auth":{"type":"none"},"models":{"opus":"m"}}}}}`, up.URL)
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, 0) }()
	var st proxy.State
	for i := 0; i < 100 && st.Port == 0; i++ {
		st, _ = proxy.Running(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := proxy.ClientSecret()
	req := testRequest(t, http.MethodPost, proxy.Base(st.Port)+"/p/up1/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("x-api-key", s)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve with a stream past the drain = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve waited on the stream past the drain budget")
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
