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
	t.Parallel()
	path := writeTranscript(t, "")
	if got := tagBlock(path); got != "" {
		t.Fatalf("tagBlock on an empty transcript = %q, want empty", got)
	}
}

func TestTagBlock_MissingTranscriptNoBlock(t *testing.T) {
	t.Parallel()
	if got := tagBlock(filepath.Join(t.TempDir(), "does-not-exist.jsonl")); got != "" {
		t.Fatalf("tagBlock on a missing transcript = %q, want empty", got)
	}
}

func TestTagBlock_WithTagsNamesAllFourDestinations(t *testing.T) {
	t.Parallel()
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

func TestAgentHarvestPrompt_UsesReferenceAndTags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	refDir := filepath.Join(dir, "skills", "reflection", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "update.md"), []byte("the procedure"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, assistantLine("NOTE: keep examples deterministic"))
	got, err := agentHarvestPrompt(path)
	if err != nil {
		t.Fatalf("agentHarvestPrompt: %v", err)
	}
	if !strings.Contains(got, "the procedure") {
		t.Errorf("prompt missing the reference text:\n%s", got)
	}
	if !strings.Contains(got, "Tagged this session") {
		t.Errorf("prompt missing the tag block:\n%s", got)
	}
}

func TestAgentHarvestPrompt_MissingReferenceErrors(t *testing.T) {
	// paths.ConfigDir() resolution is environment-driven; point CLAUDE_CONFIG_DIR at a dir with no
	// skills/reflection/references/update.md so agentHarvestPrompt fails loudly instead of running empty.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := writeTranscript(t, assistantLine("NOTE: should not appear"))
	if _, err := agentHarvestPrompt(path); err == nil {
		t.Fatalf("agentHarvestPrompt with no reference file = nil error, want one")
	}
}
