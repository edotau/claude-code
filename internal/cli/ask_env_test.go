package cli

import (
	"strings"
	"testing"
)

// A rejected --env must never echo its value: the flag parser prints the value of a failing Set.
func TestAskEnvRejectionNeverEchoesValue(t *testing.T) {
	for _, arg := range []string{"ANTHROPIC_API_KEY=supersecret", "HARNESS_VAULT_KEY=supersecret", "supersecret"} {
		code, stdout, stderr := captureOutput(t, func() int { return runAsk([]string{"--env", arg, "hi"}, false) })
		if code == 0 || strings.Contains(stdout+stderr, "supersecret") {
			t.Errorf("--env %s: code=%d, value leaked or accepted:\nstdout=%q\nstderr=%q", arg, code, stdout, stderr)
		}
	}
}
