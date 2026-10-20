package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func TestTagBlock_EmptyTranscriptNoBlock(t *testing.T) {
	path := writeTranscript(t, "")
	if got := tagBlock(path); got != "" {
		t.Fatalf("tagBlock on an empty transcript = %q, want empty", got)
	}
}

func TestTagBlock_MissingTranscriptNoBlock(t *testing.T) {
	if got := tagBlock(filepath.Join(t.TempDir(), "does-not-exist.jsonl")); got != "" {
		t.Fatalf("tagBlock on a missing transcript = %q, want empty", got)
	}
}

func TestTagBlock_WithTagsNamesAllFourDestinations(t *testing.T) {
	path := writeTranscript(t, assistantLine("DECISION: pick bbolt — rejected sqlite because no cgo"))
	got := tagBlock(path)
	if got == "" {
		t.Fatalf("tagBlock with tags present = empty, want a block")
	}
	for _, dest := range []string{"decisionLog", "progress", "activeContext", "conventions"} {
		if !strings.Contains(got, dest) {
			t.Errorf("tagBlock missing destination %q:\n%s", dest, got)
		}
	}
	if !strings.Contains(got, "DECISION: pick bbolt — rejected sqlite because no cgo") {
		t.Errorf("tagBlock missing the extracted tag text:\n%s", got)
	}
}

func TestAgentHarvestPrompt_FallbackNoCommandsFile(t *testing.T) {
	// paths.ConfigDir() resolution is environment-driven; point CLAUDE_CONFIG_DIR at a dir with no
	// commands/memory/end.md so agentHarvestPrompt takes the fallback branch (no tag block appended).
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := writeTranscript(t, assistantLine("NOTE: should not appear in the fallback prompt"))
	got := agentHarvestPrompt(path)
	if strings.Contains(got, "Tagged this session") {
		t.Fatalf("fallback prompt unexpectedly carries a tag block:\n%s", got)
	}
}
