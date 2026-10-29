package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// A router.json written at a newer schema by a live daemon must still read as running and must not be overwritten.
func TestStd12NewerSchemaRouterJSONStaysRunning(t *testing.T) {
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
	go func() { done <- Serve(ctx, 0) }()
	time.Sleep(500 * time.Millisecond)
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
