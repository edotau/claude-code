package roles

import (
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	hosttest.Cleanup()
	os.Exit(code)
}

func mustRole(t *testing.T, name string) Role {
	t.Helper()
	r, err := Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRoleVaultOverlayMovesExplore(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, rolesKey, `[{"name":"explore","agent":"opencode"}]`)
	r := mustRole(t, "explore")
	if r.Agent != "opencode" {
		t.Fatalf("overlay ignored: %+v", r)
	}
	if _, err := Lookup("fleet"); err == nil {
		t.Error("overlay replaces the table: fleet must be unknown")
	}
	if got := Legs(r, Request{Task: "x"})[0].Err; !strings.Contains(got, "explore on opencode") {
		t.Errorf("prompt-only overlay must be refused: %q", got)
	}
}
