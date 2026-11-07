package network

import (
	"strings"
	"testing"
)

func TestNetworkVSCodeHelpListsFlags(t *testing.T) {
	code, _, stderr := captureOutput(t, func() int { return Run([]string{"vscode", "-h"}, nil) })
	if code != 0 {
		t.Errorf("network vscode -h: exit %d, want 0", code)
	}
	for _, want := range []string{"network vscode", "folder", "format", "funnel", "no-tailscale", "port", "status", "stop"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("network vscode -h lacks %q: %q", want, stderr)
		}
	}
}

func TestNetworkHelpVSCodeRowFitsEightyColumns(t *testing.T) {
	_, _, stderr := captureOutput(t, func() int { return Run(nil, nil) })
	found := false
	for l := range strings.SplitSeq(stderr, "\n") {
		if strings.HasPrefix(l, "  vscode ") {
			found = true
			if n := len([]rune(l)); n > 80 {
				t.Errorf("vscode row is %d columns: %q", n, l)
			}
		}
	}
	if !found {
		t.Errorf("network help has no vscode row: %q", stderr)
	}
}

func TestNetworkVscodeServeStatusJSONWriteError(t *testing.T) {
	closedStdout(t)

	code := cmdNetworkVSCode([]string{"--status", "--format", "json"})
	if code == 0 {
		t.Errorf("network vscode status --format json on closed stdout: exit %d, want non-zero", code)
	}
}

func TestNetworkVscodeFormatValidation(t *testing.T) {
	cases := []struct {
		args []string
		name string
	}{
		{[]string{"--format", "invalid"}, "start"},
		{[]string{"--stop", "--format", "invalid"}, "stop"},
		{[]string{"--status", "--format", "invalid"}, "status"},
	}
	for _, tc := range cases {
		code, _, stderr := captureOutput(t, func() int {
			return cmdNetworkVSCode(tc.args)
		})
		if code == 0 {
			t.Errorf("network vscode %s with invalid format: exit %d, want non-zero; stderr=%q", tc.name, code, stderr)
		}
	}
}
