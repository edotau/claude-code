package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// vault env set/unset: one line per change, --dry-run writes nothing, a secret name is refused, a wrong shape is exit 2.
func TestVaultEnvSetUnset(t *testing.T) {
	vaulttest.Use(t)
	for _, c := range []struct {
		args []string
		code int
		out  string
		env  string
	}{
		{[]string{"set", "CLAUDE_REMOTE_HOST", "wks"}, 0, "env CLAUDE_REMOTE_HOST: (unset) → wks", "wks"},
		{[]string{"set", "--dry-run", "CLAUDE_REMOTE_HOST", "other"}, 0, "would set env CLAUDE_REMOTE_HOST: wks → other; nothing written", "wks"},
		{[]string{"set", "ANTHROPIC_API_KEY", "sk-x"}, 1, "use `claude-code vault set ANTHROPIC_API_KEY`", "wks"},
		{[]string{"set", "CLAUDE_REMOTE_HOST"}, 2, "usage: claude-code vault env set NAME VALUE", "wks"},
		{[]string{"unset", "CLAUDE_REMOTE_HOST"}, 0, "env CLAUDE_REMOTE_HOST: wks → (unset)", ""},
		{[]string{"unset", "CLAUDE_REMOTE_HOST"}, 0, "is not set; nothing written", ""},
		{[]string{"bogus"}, 2, "unknown subcommand", ""},
	} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVaultEnv(c.args) })
		if got := providers.CustomEnv()["CLAUDE_REMOTE_HOST"]; code != c.code || got != c.env || !strings.Contains(stdout+stderr, c.out) {
			t.Errorf("vault env %v: exit %d bucket %q output %q; want exit %d bucket %q containing %q",
				c.args, code, got, stdout+stderr, c.code, c.env, c.out)
		}
	}
}
