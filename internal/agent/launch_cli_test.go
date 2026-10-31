package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
)

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
	launch := func(args []string, o LaunchOptions) {
		t.Helper()
		plan, err := Vendor(context.Background(), "opencode", o, args)
		if err != nil || plan.Binary == "" {
			t.Fatalf("Vendor err=%v plan=%+v, want a Plan", err, plan)
		}
	}

	buf := setup("2.0.0", true)
	launch(nil, LaunchOptions{})
	if got := buf.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "opencode 2.0.0 above 1.x") {
		t.Errorf("interactive launch warning = %q, want exactly one line naming `opencode 2.0.0 above 1.x`", got)
	}

	for name, tc := range map[string]struct {
		args []string
		o    LaunchOptions
		tty  bool
		ver  string
	}{
		"--version":    {[]string{"--version"}, LaunchOptions{}, true, "2.0.0"},
		"run":          {[]string{"run", "x"}, LaunchOptions{}, true, "2.0.0"},
		"login":        {[]string{"auth", "login"}, LaunchOptions{}, true, "2.0.0"},
		"print-env":    {nil, LaunchOptions{PrintEnv: true}, true, "2.0.0"},
		"non-terminal": {nil, LaunchOptions{}, false, "2.0.0"},
		"in bounds":    {nil, LaunchOptions{}, true, "1.5.0"},
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

func TestInteractiveVendorArgs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, true},
		{[]string{"--version"}, false},
		{[]string{"-v"}, false},
		{[]string{"-V"}, false},
		{[]string{"--help"}, false},
		{[]string{"-h"}, false},
		{[]string{"run", "x"}, false},
		{[]string{"--model", "m"}, true},
		{[]string{"chat", "--version"}, false},
	}
	for _, tc := range cases {
		if got := interactiveVendorArgs(tc.args); got != tc.want {
			t.Errorf("interactiveVendorArgs(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
