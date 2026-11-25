package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
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
	secret, err := proxy.ClientSecret()
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string) int {
		req, _ := http.NewRequestWithContext(context.Background(), method, s.Base()+path, strings.NewReader("{}"))
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
	resp, err := http.Get(s.Base() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", resp.StatusCode)
	}
	sc, err := proxy.ReadScope("t", "i")
	if err != nil || proxy.Base(sc.Port) != s.Base() {
		t.Fatalf("ReadScope = %+v, %v; want port of %s", sc, err, s.Base())
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
