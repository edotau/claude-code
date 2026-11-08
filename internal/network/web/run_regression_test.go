package web

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type listeningLog struct{ urls chan string }

func (l listeningLog) Write(p []byte) (int, error) {
	if _, addr, ok := strings.Cut(string(p), "web: listening on "); ok {
		l.urls <- strings.TrimSpace(addr)
	}
	return len(p), nil
}

func runOnEphemeralPort(t *testing.T) string {
	t.Helper()
	withEmptyDispatchDir(t)
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	urls, done := make(chan string, 1), make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Addr: "127.0.0.1:0", ClaudeDir: t.TempDir(), ErrOut: listeningLog{urls}})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop after cancellation")
		}
	})
	select {
	case u := <-urls:
		return u
	case <-ctx.Done():
		t.Fatal("Run did not log its listening address")
		return ""
	}
}

func TestRunPortZeroServesAdvertisedAddress(t *testing.T) {
	u := runOnEphemeralPort(t)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(u, "http://"))
	if err != nil || port == "0" {
		t.Fatalf("listening URL = %q, want the kernel-assigned port: %v", u, err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(u + "/")
	if err != nil {
		t.Fatalf("GET advertised listening address: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 with the actual listening Host", u, resp.StatusCode)
	}
}
