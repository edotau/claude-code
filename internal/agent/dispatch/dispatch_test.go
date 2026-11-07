package dispatch

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
	vaulttest.Use(t)
	for name, args := range map[string][]string{
		"missing task": {"explore"},
		"no args":      {},
		"unknown role": {"nonesuch", "task"},
		"13 --part":    partArgs(13, "explore", "task"),
		"bad schema":   {"--output-schema", "{not json", "fleet", "task"},
		"schema file":  {"--output-schema", "/nonexistent/schema.json", "fleet", "task"},
	} {
		if code, _, _ := captureOutput(t, func() int { return Cmd(args) }); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func TestDispatchErrorMessagesNameTheCause(t *testing.T) {
	vaulttest.Use(t)
	_, _, errOut := captureOutput(t, func() int { return Cmd(partArgs(13, "explore", "task")) })
	if !strings.Contains(errOut, "13 --part") || !strings.Contains(errOut, "cap is 12") {
		t.Errorf("part cap stderr: %q", errOut)
	}
	_, _, errOut = captureOutput(t, func() int { return Cmd([]string{"--output-schema", "{not json", "fleet", "task"}) })
	if !strings.Contains(errOut, "--output-schema") {
		t.Errorf("bad schema stderr: %q", errOut)
	}
	_, _, errOut = captureOutput(t, func() int { return Cmd([]string{"nonesuch", "task"}) })
	if !strings.Contains(errOut, `unknown role "nonesuch"`) {
		t.Errorf("unknown role stderr: %q", errOut)
	}
}
