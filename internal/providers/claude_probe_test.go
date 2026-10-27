package providers

import (
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

// The probe is tri-state: a failed probe is an error (unknown), never "logged out".
func TestClaudeLoginProbeTriState(t *testing.T) {
	for _, c := range []struct {
		name, body string
		noBin      bool
		wantIn     bool
		wantPlan   string
		wantErr    bool
	}{
		{"logged in", `echo '{"loggedIn":true,"subscriptionType":"max"}'`, false, true, " (max)", false},
		{"logged out json on nonzero exit", `echo '{"loggedIn":false}'; exit 1`, false, false, "", false},
		{"bad json", `echo 'not json'`, false, false, "", true},
		{"nonzero exit no output", `exit 3`, false, false, "", true},
		{"missing binary", "", true, false, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			if !c.noBin {
				pathstest.FakeVendor(t, "claude", c.body)
			}
			in, plan, err := ClaudeLoginProbe()
			if in != c.wantIn || plan != c.wantPlan || (err != nil) != c.wantErr {
				t.Errorf("probe = %v %q %v; want %v %q err=%v", in, plan, err, c.wantIn, c.wantPlan, c.wantErr)
			}
		})
	}
}
