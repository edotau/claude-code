package launchcli

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
)

// headless is the only launch that carries the setup-token: -p and --print, in any position.
func TestHeadless(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"-p", "hi"}, true},
		{[]string{"hi", "--print"}, true},
		{nil, false},
		{[]string{"-c"}, false},
		{[]string{"--settings", "x"}, false},
	} {
		if got := IsHeadless(tc.args); got != tc.want {
			t.Errorf("agent.IsHeadless(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestParseArgs(t *testing.T) {
	o, rest, err := ParseArgs([]string{"--profile=openai", "-p", "hi", "--model", "gpt-5", "--print-env", "--", "--model", "x"}, true)
	if err != nil || o.Provider != "openai" || o.Model != "gpt-5" || !o.PrintEnv {
		t.Fatalf("%+v %v", o, err)
	}
	if !slices.Equal(rest, []string{"-p", "hi", "--model", "x"}) {
		t.Errorf("rest %q", rest)
	}
	if _, _, err := ParseArgs([]string{"--profile"}, true); err == nil {
		t.Error("missing value accepted")
	}
}

// --keep-env takes a comma list, repeatable, and accepts both the "--name value" and "--name=value" shapes.
func TestParseArgsKeepEnv(t *testing.T) {
	o, _, err := ParseArgs([]string{"--keep-env", "LANG,LC_ALL", "--keep-env=HOME"}, true)
	if err != nil || !slices.Equal(o.KeepEnv, []string{"LANG", "LC_ALL", "HOME"}) {
		t.Fatalf("KeepEnv = %q, err %v", o.KeepEnv, err)
	}
	if _, _, err := ParseArgs([]string{"--keep-env"}, true); err == nil {
		t.Error("missing value accepted")
	}
}

// A credential name in --keep-env is refused whether or not a "--" follows; args after "--" pass through untouched.
func TestParseArgsKeepEnvCredentialRefusedWithAndWithoutDoubleDash(t *testing.T) {
	for _, args := range [][]string{
		{"--keep-env", "ANTHROPIC_API_KEY", "exec", "foo"},
		{"--keep-env", "ANTHROPIC_API_KEY", "--", "exec", "foo"},
	} {
		if _, _, err := ParseArgs(args, false); err == nil || !strings.Contains(err.Error(), "a credential name cannot be kept") {
			t.Errorf("ParseArgs(%v) err = %v, want the credential refusal", args, err)
		}
	}
	o, rest, err := ParseArgs([]string{"--keep-env", "FOO_HOME", "--", "exec", "--keep-env", "ANTHROPIC_API_KEY"}, false)
	if err != nil || !slices.Equal(o.KeepEnv, []string{"FOO_HOME"}) || !slices.Equal(rest, []string{"exec", "--keep-env", "ANTHROPIC_API_KEY"}) {
		t.Errorf("benign name: keep=%v rest=%v err=%v; child args after -- must pass through unchanged", o.KeepEnv, rest, err)
	}
}

// Every removed flag is a parse error carrying its hint; --profile stays valid.
func TestParseLaunchArgsRemovedFlags(t *testing.T) {
	for flag, want := range map[string]string{
		"--provider": "--profile", "--env": "--profile", "--tier": "--model", "--router": "removed",
	} {
		for _, claude := range []bool{true, false} {
			if _, _, err := ParseLaunchArgs([]string{flag, "x"}, claude); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("ParseLaunchArgs(%s, claude=%v) = %v, want hint %q", flag, claude, err, want)
			}
		}
	}
	o, rest, err := ParseLaunchArgs([]string{"--profile", "openai", "-p", "hi"}, true)
	if err != nil || o.Provider != "openai" || !slices.Equal(rest, []string{"-p", "hi"}) {
		t.Errorf("--profile control: %+v %q %v", o, rest, err)
	}
}

// A launch refusal (*agent.UsageError, even wrapped) prints its message and exits 2, not the generic 1.
func TestRunPlanUsageErrorExitsTwo(t *testing.T) {
	err := fmt.Errorf("launch: %w", &UsageError{Msg: "not a harness profile"})
	if code := Run("claude", agent.Plan{}, agent.LaunchOptions{}, err); code != 2 {
		t.Fatalf("Run(UsageError) = %d, want 2", code)
	}
}
