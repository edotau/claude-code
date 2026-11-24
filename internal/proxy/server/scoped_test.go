package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proxy"
)

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
