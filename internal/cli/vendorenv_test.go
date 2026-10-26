package cli

import (
	"strings"
	"testing"
)

// Bare `<vendor> env` names the row's account, and says so plainly when the active profile has none.
func TestCmdVendorEnvShowsAccount(t *testing.T) {
	profileFixture(t, "work")
	cases := []struct{ vendor, want string }{
		{"codex", "codex: acme\n"},
		{"opencode", "opencode: no account in the active profile"},
	}
	for _, c := range cases {
		code, out, _ := captureOutput(t, func() int { return cmdVendorEnv(c.vendor, nil) })
		if code != 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: code=%d out=%q, want %q", c.vendor, code, out, c.want)
		}
	}
}
