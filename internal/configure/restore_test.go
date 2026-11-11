package configure

import (
	"bytes"
	"strings"
	"testing"
)

func TestRevertTrimsCommaListAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var out bytes.Buffer
	if err := Revert(&out, "codex, gemini", true); err != nil {
		t.Fatalf("Revert() = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "no backup to revert") {
		t.Errorf("Revert output = %q, want missing-backup messages", out.String())
	}
	if err := Revert(&out, "codex, nope", true); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("Revert() error = %v, want unknown-agent error for nope", err)
	}
}
