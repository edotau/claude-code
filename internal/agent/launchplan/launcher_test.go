package launchplan_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/launchplan"
	_ "github.com/edotau/claude-code/internal/claude" // the vendor roster registers for the whole test binary
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	_ "github.com/edotau/claude-code/internal/opencode"
)

// A credential name in --keep-env is refused whether or not a "--" follows; args after "--" pass through untouched.
func TestParseArgsKeepEnvCredentialRefusedWithAndWithoutDoubleDash(t *testing.T) {
	for _, args := range [][]string{
		{"--keep-env", "ANTHROPIC_API_KEY", "exec", "foo"},
		{"--keep-env", "ANTHROPIC_API_KEY", "--", "exec", "foo"},
	} {
		if _, _, err := launchplan.ParseArgs(args, false); err == nil || !strings.Contains(err.Error(), "a credential name cannot be kept") {
			t.Errorf("launchplan.ParseArgs(%v) err = %v, want the credential refusal", args, err)
		}
	}
	o, rest, err := launchplan.ParseArgs([]string{"--keep-env", "FOO_HOME", "--", "exec", "--keep-env", "ANTHROPIC_API_KEY"}, false)
	if err != nil || !slices.Equal(o.KeepEnv, []string{"FOO_HOME"}) || !slices.Equal(rest, []string{"exec", "--keep-env", "ANTHROPIC_API_KEY"}) {
		t.Errorf("benign name: keep=%v rest=%v err=%v; child args after -- must pass through unchanged", o.KeepEnv, rest, err)
	}
}
