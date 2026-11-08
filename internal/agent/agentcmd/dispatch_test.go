package agentcmd

import (
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"strings"
	"testing"
)

// partArgs is n repeated --part flags followed by tail.
func partArgs(n int, tail ...string) []string {
	var args []string
	for range n {
		args = append(args, "--part", "q")
	}
	return append(args, tail...)
}

func TestDispatchUsageErrorsExit2(t *testing.T) {
	vaulttest.UseFreshKey(t)
	for name, args := range map[string][]string{
		"missing task": {"explore"},
		"no args":      {},
		"unknown role": {"nonesuch", "task"},
		"13 --part":    partArgs(13, "explore", "task"),
		"bad schema":   {"--output-schema", "{not json", "fleet", "task"},
		"schema file":  {"--output-schema", "/nonexistent/schema.json", "fleet", "task"},
	} {
		if code, _, _ := hosttest.CaptureOutput(t, func() int { return dispatchCmd(args) }); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func TestDispatchErrorMessagesNameTheCause(t *testing.T) {
	vaulttest.UseFreshKey(t)
	_, _, errOut := hosttest.CaptureOutput(t, func() int { return dispatchCmd(partArgs(13, "explore", "task")) })
	if !strings.Contains(errOut, "13 --part") || !strings.Contains(errOut, "cap is 12") {
		t.Errorf("part cap stderr: %q", errOut)
	}
	_, _, errOut = hosttest.CaptureOutput(t, func() int { return dispatchCmd([]string{"--output-schema", "{not json", "fleet", "task"}) })
	if !strings.Contains(errOut, "--output-schema") {
		t.Errorf("bad schema stderr: %q", errOut)
	}
	_, _, errOut = hosttest.CaptureOutput(t, func() int { return dispatchCmd([]string{"nonesuch", "task"}) })
	if !strings.Contains(errOut, `unknown role "nonesuch"`) {
		t.Errorf("unknown role stderr: %q", errOut)
	}
}
