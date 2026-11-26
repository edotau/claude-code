package cli

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// vault set env / rm env: one line per change, --dry-run writes nothing, a secret name is refused, bad shape exits 2.
func TestVaultEnvSetUnset(t *testing.T) {
	vaulttest.Use(t)
	for _, c := range []struct {
		args []string
		code int
		out  string
		env  string
	}{
		{[]string{"set", "env", "CLAUDE_REMOTE_HOST", "devhost"}, 0, "env CLAUDE_REMOTE_HOST set to devhost (was not set)", "devhost"},
		{[]string{"set", "env", "--dry-run", "CLAUDE_REMOTE_HOST", "other"}, 0, "dry run: env CLAUDE_REMOTE_HOST changed from devhost to other; nothing written", "devhost"},
		{[]string{"set", "env", "ANTHROPIC_API_KEY", "qZjnpdhpfohm"}, 1, "use `claude-code vault set secrets ANTHROPIC_API_KEY`", "devhost"},
		{[]string{"set", "env", "CLAUDE_REMOTE_HOST"}, 2, "usage: claude-code vault set", "devhost"},
		{[]string{"rm", "env", "CLAUDE_REMOTE_HOST"}, 2, "pass --yes to remove it", "devhost"},
		{[]string{"set", "env", "CLAUDE_REMOTE_HOST", "x"}, 2, "pass --yes to overwrite it", "devhost"},
		{[]string{"rm", "env", "-y", "CLAUDE_REMOTE_HOST"}, 0, "env CLAUDE_REMOTE_HOST removed (was devhost)", ""},
		{[]string{"rm", "env", "CLAUDE_REMOTE_HOST"}, 0, "is not set; nothing written", ""},
		{[]string{"env", "set", "CLAUDE_REMOTE_HOST", "x"}, 2, "use claude-code vault set env", ""},
	} {
		code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return cmdVault(c.args) })
		if got := providers.CustomEnv()["CLAUDE_REMOTE_HOST"]; code != c.code || got != c.env || !strings.Contains(stdout+stderr, c.out) {
			t.Errorf("vault %v: exit %d bucket %q output %q; want exit %d bucket %q containing %q",
				c.args, code, got, stdout+stderr, c.code, c.env, c.out)
		}
	}
}
