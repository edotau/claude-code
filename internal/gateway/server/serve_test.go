package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A test binary must never bind the live port: Serve's gateway.Reclaim would SIGTERM the user's running router.
func TestServeRefusesDefaultPortUnderTest(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // without the guard Serve stops at ctx.Err, so a regression fails here instead of binding the port
	if err := Serve(ctx, gateway.DefaultPort); !errors.Is(err, gateway.ErrTestBinary) {
		t.Fatalf("Serve(gateway.DefaultPort) under test = %v, want gateway.ErrTestBinary", err)
	}
}

// A router.json written at a newer schema by a live daemon must still read as running and must not be overwritten.
func TestNewerSchemaRouterJSONStaysRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().
			Set(gateway.PIDHeader, strconv.Itoa(os.Getpid()))
			// a live router names its pid; Running rejects a pid-less non-claude-code holder
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	doc := fmt.Sprintf(`{"schema":2,"pid":%d,"port":%d,"started":"2026-01-01T00:00:00Z","extra":"newer"}`, os.Getpid(),
		port)
	writeNewerRouterJSON(t, doc)
	defer os.Remove(filepath.Join(paths.StateDir(), "router.json"))

	st, err := gateway.ReadState()
	if err != nil || st.PID != os.Getpid() || st.Port != port {
		t.Fatalf("gateway.ReadState = %+v, %v; want the schema-2 pid/port", st, err)
	}
	// Stop's only gate is gateway.Running; calling Stop itself would SIGTERM this test process.
	if _, ok := gateway.Running(context.Background()); !ok {
		t.Fatal("gateway.Running = false for a live pid with a healthy port; Stop would return ErrNotRunning")
	}

	serveAndStop(t)
	got, err := os.ReadFile(filepath.Join(paths.StateDir(), "router.json"))
	if err != nil || string(got) != doc {
		t.Fatalf("router.json = %q, %v; want the newer build's file untouched", got, err)
	}
}

// writeNewerRouterJSON writes doc as the state dir's router.json, creating the directory.
func writeNewerRouterJSON(t *testing.T, doc string) {
	t.Helper()
	file := filepath.Join(paths.StateDir(), "router.json")
	if err := os.MkdirAll(file[:len(file)-len("/router.json")], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// serveAndStop runs Serve on a free port until it answers, then cancels it and waits for it to return.
func serveAndStop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	servePort := pl.Addr().(*net.TCPAddr).Port
	_ = pl.Close()
	go func() { done <- Serve(ctx, servePort) }()
	waitServing(t, servePort)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// TestServeRefusesHeldPort: a held port is never swapped for a random one; the error names the holder.
func TestServeRefusesHeldPort(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
		want string
	}{
		{
			"foreign",
			func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			"held by another process",
		},
		{
			"older router",
			func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) },
			"reports no pid",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			port := occupy(t, c.h)
			err := Serve(context.Background(), port)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Serve on held :%d = %v, want %q", port, err, c.want)
			}
		})
	}
}

// An explicit busy --port is never moved: Serve errors, binds nothing and writes no router.json.
func TestServeExplicitBusyPortErrors(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	tcpPort, _ := plainListener(t)
	pidless := occupy(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	for name, port := range map[string]int{"plain tcp": tcpPort, "pid-less router": pidless} {
		if err := Serve(context.Background(), port); err == nil {
			t.Errorf("%s: Serve on busy explicit port returned nil, want an error", name)
		}
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "router.json")); !os.IsNotExist(err) {
		t.Errorf("router.json written after refused Serve: %v", err)
	}
}

// occupy serves h on a loopback port and returns the port.
func occupy(t *testing.T, h http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

// plainListener holds a loopback port with a bare TCP listener that never speaks HTTP.
func plainListener(t *testing.T) (port int, isClosed func() bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() bool {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	}
}

func waitServing(t *testing.T, servePort int) {
	// Any HTTP response proves srv.Serve is running, which happens after Serve's router.json decision.
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", servePort))
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Serve never became ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

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
	base, err := gateway.Ensure(context.Background())
	if err != nil || base != gateway.Base(st.Port) {
		t.Fatalf("gateway.Ensure = %q, %v", base, err)
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
func waitRouterHealthy(t *testing.T) gateway.State {
	t.Helper()
	var st gateway.State
	for range 100 {
		if s, ok := gateway.Running(context.Background()); ok {
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
	s1, _ := gateway.ClientSecret()
	s2, _ := gateway.ClientSecret()
	_, err := os.Stat(filepath.Join(paths.StateDir(), "router.secret"))
	if !os.IsNotExist(err) || s1 == "" || s1 != s2 || len(s1) != 64 {
		t.Fatalf("secret %q/%q (vault-sealed, no file) stat err %v", s1, s2, err)
	}
}

// A stream past the drain is closed and Serve still returns nil: exiting 1 there reported every busy stop as a failure.
func TestServeClosesStreamsPastTheDrain(t *testing.T) {
	old := gateway.ShutdownDrain
	gateway.ShutdownDrain = 100 * time.Millisecond
	t.Cleanup(func() { gateway.ShutdownDrain = old })
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
	overlay := fmt.Sprintf(
		`{"default":"up1","providers":{"anthropic":{"up1":{"protocol":"anthropic","base_url":%q,"auth":{"type":"none"},"models":{"opus":"m"}}}}}`,
		up.URL)
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, 0) }()
	var st gateway.State
	for i := 0; i < 100 && st.Port == 0; i++ {
		st, _ = gateway.Running(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	s, _ := gateway.ClientSecret()
	req := testRequest(t, http.MethodPost, gateway.Base(st.Port)+"/p/up1/v1/messages",
		strings.NewReader(`{"model":"m","stream":true}`))
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
	daemon := gateway.State{PID: os.Getppid(), Port: 1, Started: time.Now().UTC()} // a live pid on another port
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
	if cur, err := gateway.ReadState(); err != nil || cur.PID != daemon.PID || cur.Port != daemon.Port {
		t.Fatalf("while serving, router.json = %+v, %v; want the daemon's %+v", cur, err, daemon)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if cur, err := gateway.ReadState(); err != nil || cur.PID != daemon.PID {
		t.Fatalf("after the side proxy exits, router.json = %+v, %v; want the daemon's kept", cur, err)
	}
}
