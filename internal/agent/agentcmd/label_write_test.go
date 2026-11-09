package agentcmd

import (
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestAgentsLabelStdoutFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	hosttest.ClosedStdout(t)
	if code := cmdAgentsLabel([]string{"Plan", "--session-model", "databricks-claude-opus-5-5"}); code != 1 {
		t.Fatalf("exit = %d, want 1 when writing the prefix fails", code)
	}
}
