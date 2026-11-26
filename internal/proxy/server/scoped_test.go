package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/proxy"
)

func TestScopedDrainIfIdle(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	s, err := Open(context.Background(), proxy.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := proxy.Base(s.port)
	secret, err := proxy.ClientSecret()
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string) int {
		req, _ := http.NewRequestWithContext(context.Background(), method, base+path, strings.NewReader("{}"))
		req.Header.Set(proxy.ClientHeader, secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := do(http.MethodPost, proxy.DrainPath); got != http.StatusOK {
		t.Fatalf("drain = %d, want 200", got)
	}
	if got := do(http.MethodGet, "/healthz"); got != http.StatusServiceUnavailable {
		t.Fatalf("healthz while draining = %d, want 503", got)
	}
	if got := do(http.MethodPost, "/p/x/v1/messages"); got != http.StatusServiceUnavailable {
		t.Fatalf("request while draining = %d, want 503", got)
	}
	busy := &Router{}
	busy.inflight.Store(1)
	if drainIfIdle(busy) || busy.draining.Load() {
		t.Fatal("a busy router drained or was left draining")
	}
}

// A scoped proxy serves and records itself without ever writing proxy.json.
func TestOpenLeavesNoProxyJSON(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	stateJSON := filepath.Join(paths.StateDir(), "proxy.json")
	s, err := Open(context.Background(), proxy.Scope{Team: "t", ID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := proxy.Base(s.port)
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", resp.StatusCode)
	}
	sc, err := proxy.ReadScope("t", "i")
	if err != nil || proxy.Base(sc.Port) != base {
		t.Fatalf("ReadScope = %+v, %v; want port of %s", sc, err, base)
	}
	if _, err := os.Stat(stateJSON); !os.IsNotExist(err) {
		t.Fatalf("proxy.json exists while open: %v", err)
	}
	s.Close()
	if _, err := proxy.ReadScope("t", "i"); err == nil {
		t.Error("scope file survives Close")
	}
	if _, err := os.Stat(stateJSON); !os.IsNotExist(err) {
		t.Errorf("proxy.json exists after Close: %v", err)
	}
}

// Close returns once nothing is in flight, even with a stalled connection the router never counted.
func TestScopedCloseIdleReturnsFast(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Open(ctx, proxy.Scope{Team: "t", ID: "i"})
	if err != nil {
		t.Fatal(err)
	}
	base := proxy.Base(s.port)
	resp, err := http.Get(base + "/healthz") // leaves one idle keepalive connection
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	c, err := net.Dial("tcp", base[len("http://"):]) // a gate-answered request whose body never arrives
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("POST /healthz HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 100\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	cancel()
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within 3s with nothing in flight")
	}
}

func BenchmarkOpen(b *testing.B) {
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	b.Setenv("HARNESS_VAULT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	for i := 0; i < b.N; i++ {
		s, err := Open(context.Background(), proxy.Scope{Team: "t", ID: "i"})
		if err != nil {
			b.Fatal(err)
		}
		s.Close()
	}
}
