package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// vault overlay unset: off a terminal it needs --yes, --dry-run writes nothing, an unknown key is refused.
func TestVaultOverlayUnset(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "proxy-teams", `{"schema":1}`)
	for _, c := range []struct {
		args []string
		code int
		out  string
		left bool
	}{
		{[]string{"unset", "providers", "--yes"}, 1, "not a removable overlay key", true},
		{[]string{"unset", "proxy-teams"}, 2, "pass --yes", true},
		{[]string{"unset", "--dry-run", "proxy-teams"}, 0, "would be removed", true},
		{[]string{"ls"}, 0, "proxy-teams  12 bytes", true},
		{[]string{"unset", "--yes", "proxy-teams"}, 0, "overlay proxy-teams removed (was 12 bytes)", false},
		{[]string{"unset", "--yes", "proxy-teams"}, 0, "not set; nothing written", false},
	} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVaultOverlay(c.args) })
		var left bool
		_ = vault.View(func(tx *vault.Tx) error { _, left = tx.Get(vault.Overlay, "proxy-teams"); return nil })
		if code != c.code || left != c.left || !strings.Contains(stdout+stderr, c.out) {
			t.Errorf("vault overlay %v: exit %d left %v output %q; want exit %d left %v containing %q",
				c.args, code, left, stdout+stderr, c.code, c.left, c.out)
		}
	}
}

// vault overlay set: an over-cap proxy-teams document is refused and nothing is written.
func TestVaultOverlaySetRefusesOverCap(t *testing.T) {
	vaulttest.Use(t)
	doc := filepath.Join(t.TempDir(), "teams.json")
	bad := `{"schema":1,"teams":[{"name":"phase","kinds":["group"],"host":"phase","lifetime":"phase",` +
		`"reserved":1024,"borrow":"never","fallback":["shared"]}]}`
	if err := os.WriteFile(doc, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return cmdVaultOverlay([]string{"set", "proxy-teams", "--file", doc}) })
	var stored bool
	_ = vault.View(func(tx *vault.Tx) error { _, stored = tx.Get(vault.Overlay, "proxy-teams"); return nil })
	if code == 0 || stored || !strings.Contains(stderr, "nothing written") {
		t.Errorf("exit %d stored %v stderr %q; want refusal with nothing stored", code, stored, stderr)
	}
}
