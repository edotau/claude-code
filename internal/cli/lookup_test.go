package cli

import (
	"strings"
	"testing"
)

func TestSecretsVerbRemoved(t *testing.T) {
	useVaultTest(t)
	if lookup("secrets") != nil {
		t.Fatal("removed secrets verb is still registered")
	}
	code, _, stderr := captureOutput(t, func() int { return Run([]string{"claude-code", "secrets"}) })
	if code != 2 || !strings.Contains(stderr, `unknown command "secrets"`) {
		t.Errorf("removed verb: exit %d, stderr=%q", code, stderr)
	}
}
