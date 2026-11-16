package configure

import (
	"bytes"
	"os"
	"path/filepath"
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

// TestRestoreOutSkipsAgentRender: --out resets only that file; the agent-config render is never attempted.
func TestRestoreOutSkipsAgentRender(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	out := filepath.Join(t.TempDir(), "scratch.json")
	var buf bytes.Buffer
	if err := Restore(&buf, "", out, false); err != nil {
		t.Fatalf("Restore() = %v", err)
	}
	if got := buf.String(); got != "restored "+out+"\n" {
		t.Errorf("Restore output = %q, want only the restored line", got)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("--out file not written: %v", err)
	}
}
