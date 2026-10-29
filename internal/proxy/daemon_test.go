package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"
)

// TestDaemonCommandArgs: the detached daemon re-enters the binary as `gateway proxy --serve`, not the removed `router serve`.
func TestDaemonCommandArgs(t *testing.T) {
	cmd := daemonCommand("/bin/claude-code")
	if want := []string{"/bin/claude-code", "gateway", "proxy", "--serve"}; !slices.Equal(cmd.Args, want) {
		t.Fatalf("daemon args = %v, want %v", cmd.Args, want)
	}
}

// A test binary must never bind the live port: Serve's reclaim would SIGTERM the user's running router.
func TestServeRefusesDefaultPortUnderTest(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // without the guard Serve stops at ctx.Err, so a regression fails here instead of binding the port
	if err := Serve(ctx, DefaultPort); !errors.Is(err, errTestBinary) {
		t.Fatalf("Serve(DefaultPort) under test = %v, want errTestBinary", err)
	}
}

// A router.json written at a newer schema by a live daemon must still read as running and must not be overwritten.
func TestNewerSchemaRouterJSONStaysRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	doc := fmt.Sprintf(`{"schema":2,"pid":%d,"port":%d,"started":"2026-01-01T00:00:00Z","extra":"newer"}`, os.Getpid(), port)
	if err := os.MkdirAll(stateFile()[:len(stateFile())-len("/router.json")], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile(), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stateFile())

	st, err := ReadState()
	if err != nil || st.PID != os.Getpid() || st.Port != port {
		t.Fatalf("ReadState = %+v, %v; want the schema-2 pid/port", st, err)
	}
	// Stop's only gate is Running; calling Stop itself would SIGTERM this test process.
	if _, ok := Running(context.Background()); !ok {
		t.Fatal("Running = false for a live pid with a healthy port; Stop would return ErrNotRunning")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	servePort := pl.Addr().(*net.TCPAddr).Port
	_ = pl.Close()
	go func() { done <- Serve(ctx, servePort) }()
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
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	got, err := os.ReadFile(stateFile())
	if err != nil || string(got) != doc {
		t.Fatalf("router.json = %q, %v; want the newer build's file untouched", got, err)
	}
}
