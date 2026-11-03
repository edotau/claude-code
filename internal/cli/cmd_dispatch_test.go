package cli

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
	} {
		if code, _, _ := captureOutput(t, func() int { return cmdDispatch(args) }); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
}

func TestDispatchErrorMessagesNameTheCause(t *testing.T) {
	vaulttest.Use(t)
	_, _, errOut := captureOutput(t, func() int { return cmdDispatch(partArgs(13, "explore", "task")) })
	if !strings.Contains(errOut, "13 --part") || !strings.Contains(errOut, "cap is 12") {
		t.Errorf("part cap stderr: %q", errOut)
	}
	_, _, errOut = captureOutput(t, func() int { return cmdDispatch([]string{"nonesuch", "task"}) })
	if !strings.Contains(errOut, `unknown role "nonesuch"`) {
		t.Errorf("unknown role stderr: %q", errOut)
	}
}
