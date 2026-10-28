package launch

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestDoctorAgentVersionRows(t *testing.T) {
	hermetic(t, "")
	pathstest.FakeVendor(t, "copilot", "echo 'GitHub Copilot CLI 1.0.100.'")
	checks, _ := DoctorChecks()
	c := findCheck(checks, "agent version copilot")
	if c == nil {
		t.Fatal("no `agent version copilot` row")
	}
	if !strings.HasPrefix(c.Detail, "warn 1.0.100 below 1.0.104") || c.Hard {
		t.Errorf("row = %+v, want detail `warn 1.0.100 below 1.0.104...` and Hard=false", *c)
	}

	hermetic(t, "")
	t.Setenv("PATH", t.TempDir())
	checks, _ = DoctorChecks()
	c = findCheck(checks, "agent version copilot")
	if c == nil || c.Detail != "not installed" || c.Hard {
		t.Errorf("missing binary row = %+v, want `not installed`, non-hard", c)
	}
}

func TestVendorLaunchVersionWarning(t *testing.T) {
	setup := func(version string, tty bool) *bytes.Buffer {
		hermetic(t, "")
		t.Setenv("OPENCODE_API_KEY", "test-key") // an own-login route, so the plan is a plain launch
		pathstest.FakeVendor(t, "opencode", "echo 'opencode "+version+"'")
		var buf bytes.Buffer
		oldTTY, oldOut := agentVersionTTY, versionWarnOut
		agentVersionTTY, versionWarnOut = func() bool { return tty }, &buf
		t.Cleanup(func() { agentVersionTTY, versionWarnOut = oldTTY, oldOut })
		return &buf
	}
	launch := func(args []string, o Options) {
		t.Helper()
		plan, err := Vendor(context.Background(), "opencode", o, args)
		if err != nil || plan.Binary == "" {
			t.Fatalf("Vendor err=%v plan=%+v, want a Plan", err, plan)
		}
	}

	buf := setup("2.0.0", true)
	launch(nil, Options{})
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "opencode 2.0.0 above 1.x") {
		t.Errorf("interactive launch warning = %q, want exactly one line naming `opencode 2.0.0 above 1.x`", got)
	}

	for name, tc := range map[string]struct {
		args []string
		o    Options
		tty  bool
		ver  string
	}{
		"--version":    {[]string{"--version"}, Options{}, true, "2.0.0"},
		"run":          {[]string{"run", "x"}, Options{}, true, "2.0.0"},
		"login":        {[]string{"auth", "login"}, Options{}, true, "2.0.0"},
		"print-env":    {nil, Options{PrintEnv: true}, true, "2.0.0"},
		"non-terminal": {nil, Options{}, false, "2.0.0"},
		"in bounds":    {nil, Options{}, true, "1.5.0"},
	} {
		t.Run(name, func(t *testing.T) {
			buf := setup(tc.ver, tc.tty)
			launch(tc.args, tc.o)
			if buf.Len() != 0 {
				t.Errorf("warned %q, want silence", buf.String())
			}
		})
	}
}
