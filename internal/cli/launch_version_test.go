package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

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
