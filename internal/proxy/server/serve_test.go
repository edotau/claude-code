package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proxy"
)

// A test binary must never bind the live port: Serve's proxy.Reclaim would SIGTERM the user's running router.
func TestServeRefusesDefaultPortUnderTest(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // without the guard Serve stops at ctx.Err, so a regression fails here instead of binding the port
	if err := Serve(ctx, proxy.DefaultPort); !errors.Is(err, proxy.ErrTestBinary) {
		t.Fatalf("Serve(proxy.DefaultPort) under test = %v, want proxy.ErrTestBinary", err)
	}
}

// A router.json written at a newer schema by a live daemon must still read as running and must not be overwritten.
func TestNewerSchemaRouterJSONStaysRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(proxy.PIDHeader, strconv.Itoa(os.Getpid())) // a live router names its pid; Running rejects a pid-less non-claude-code holder
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	doc := fmt.Sprintf(`{"schema":2,"pid":%d,"port":%d,"started":"2026-01-01T00:00:00Z","extra":"newer"}`, os.Getpid(), port)
	writeNewerRouterJSON(t, doc)
	defer os.Remove(filepath.Join(paths.StateDir(), "router.json"))

	st, err := proxy.ReadState()
	if err != nil || st.PID != os.Getpid() || st.Port != port {
		t.Fatalf("proxy.ReadState = %+v, %v; want the schema-2 pid/port", st, err)
	}
	// Stop's only gate is proxy.Running; calling Stop itself would SIGTERM this test process.
	if _, ok := proxy.Running(context.Background()); !ok {
		t.Fatal("proxy.Running = false for a live pid with a healthy port; Stop would return ErrNotRunning")
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
		{"foreign", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }, "held by another process"},
		{"older router", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) }, "reports no pid"},
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
