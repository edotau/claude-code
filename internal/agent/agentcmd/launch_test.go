package agentcmd

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/launchplan"

	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
)

// Every removed flag is a parse error carrying its hint; --profile stays valid.
func TestParseLaunchArgsRemovedFlags(t *testing.T) {
	for flag, want := range map[string]string{
		"--provider": "--profile", "--env": "--profile", "--tier": "--model", "--router": "removed",
	} {
		for _, claude := range []bool{true, false} {
			if _, _, err := parseLaunchArgs([]string{flag, "x"}, claude); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("parseLaunchArgs(%s, claude=%v) = %v, want hint %q", flag, claude, err, want)
			}
		}
	}
	o, rest, err := parseLaunchArgs([]string{"--profile", "openai", "-p", "hi"}, true)
	if err != nil || o.Provider != "openai" || !slices.Equal(rest, []string{"-p", "hi"}) {
		t.Errorf("--profile control: %+v %q %v", o, rest, err)
	}
}

func TestWarnVersion(t *testing.T) {
	setup := func(version string, tty bool) (string, *bytes.Buffer) {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		bin := pathstest.FakeVendor(t, "opencode", "echo 'opencode "+version+"'")
		var buf bytes.Buffer
		oldTTY, oldOut := agentVersionTTY, versionWarnOut
		agentVersionTTY, versionWarnOut = func() bool { return tty }, &buf
		t.Cleanup(func() { agentVersionTTY, versionWarnOut = oldTTY, oldOut })
		return bin, &buf
	}

	bin, buf := setup("2.0.0", true)
	warnVersion("opencode", bin, agent.LaunchOptions{}, nil)
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "opencode 2.0.0 above 1.x") {
		t.Errorf("interactive launch warning = %q, want exactly one line naming `opencode 2.0.0 above 1.x`", got)
	}

	for name, tc := range map[string]struct {
		args []string
		o    agent.LaunchOptions
		tty  bool
		ver  string
	}{
		"--version":    {[]string{"--version"}, agent.LaunchOptions{}, true, "2.0.0"},
		"run":          {[]string{"run", "x"}, agent.LaunchOptions{}, true, "2.0.0"},
		"login":        {[]string{"auth", "login"}, agent.LaunchOptions{}, true, "2.0.0"},
		"print-env":    {nil, agent.LaunchOptions{PrintEnv: true}, true, "2.0.0"},
		"non-terminal": {nil, agent.LaunchOptions{}, false, "2.0.0"},
		"in bounds":    {nil, agent.LaunchOptions{}, true, "1.5.0"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, buf := setup(tc.ver, tc.tty)
			warnVersion("opencode", bin, tc.o, tc.args)
			if buf.Len() != 0 {
				t.Errorf("warned %q, want silence", buf.String())
			}
		})
	}
}

func TestTakeAsBeatsProfileIdentity(t *testing.T) {
	as, rest, err := providers.TakeAs([]string{"--model", "x", "--as", "sp", "--", "--as", "user"})
	if err != nil || as != providers.IdentitySP {
		t.Fatalf("as=%q err=%v", as, err)
	}
	if len(rest) != 5 || rest[3] != "--as" {
		t.Fatalf("rest=%v", rest)
	}
	// the flag wins over a profile set to user
	if got, _ := providers.ResolveIdentity(providers.IdentityUser, as); got != providers.IdentitySP {
		t.Fatalf("resolved %q", got)
	}
	if _, _, err := providers.TakeAs([]string{"--as=bogus"}); err == nil {
		t.Fatal("bogus accepted")
	}
	if err := client.ValidateAs("sp", "codex"); err == nil {
		t.Fatal("--as accepted for codex")
	}
}

// A launch refusal (*agent.UsageError, even wrapped) prints its message and exits 2, not the generic 1.
func TestRunPlanUsageErrorExitsTwo(t *testing.T) {
	err := fmt.Errorf("launch: %w", &launchplan.UsageError{Msg: "not a harness profile"})
	if code := runPlan("claude", agent.Plan{}, agent.LaunchOptions{}, err); code != 2 {
		t.Fatalf("runPlan(UsageError) = %d, want 2", code)
	}
}

// `<alias> --help` runs claude's own help; a profile with no model for the alias must not refuse it.
func TestLaunchClaudeHelpSkipsModelLookup(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pathstest.FakeVendor(t, "claude", `[ "$1" = --help ] && echo FAKE-CLAUDE-HELP`)
	code, out, errOut := captureOutput(t, func() int { return launchClaude("nosuchalias", []string{"--help"}) })
	if code != 0 || !strings.Contains(out, "FAKE-CLAUDE-HELP") {
		t.Fatalf("launchClaude(--help) = %d, stdout %q, stderr %q; want 0 and claude's help", code, out, errOut)
	}
}
