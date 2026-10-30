package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestAgentUpdatePrompt_UsesReferenceAndTags(t *testing.T) {
	path := writeTranscript(t, assistantLine("NOTE: keep examples deterministic"))
	got, err := agentUpdatePrompt(path, "")
	if err != nil {
		t.Fatalf("agentUpdatePrompt: %v", err)
	}
	if !strings.Contains(got, "Judgment Residue") {
		t.Errorf("prompt missing the embedded update reference:\n%s", got)
	}
	if !strings.Contains(got, "Tagged this session") {
		t.Errorf("prompt missing the tag block:\n%s", got)
	}
}

// TestAgentUpdatePrompt_UnknownVerbErrors: Reference errors on an unknown verb; agentUpdatePrompt
// must surface that rather than running on an empty prompt. "update" itself is now always embedded,
// so this exercises the error path through a verb that will never exist.
func TestAgentUpdatePrompt_UnknownVerbErrors(t *testing.T) {
	if _, err := Reference("does-not-exist"); err == nil {
		t.Fatal("Reference(\"does-not-exist\") = nil error, want one")
	}
}

// TestAgentUpdatePrompt_WithCWDAppendsGitActivity: a transcript with a parseable timestamp and a
// real git cwd gets a "git activity" block with the session's commits/touched files appended.
func TestAgentUpdatePrompt_WithCWDAppendsGitActivity(t *testing.T) {
	cwd := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(cwd, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "seed commit")

	ts := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	path := writeTranscript(t, `{"timestamp":"`+ts+`"}`)
	got, err := agentUpdatePrompt(path, cwd)
	if err != nil {
		t.Fatalf("agentUpdatePrompt: %v", err)
	}
	if !strings.Contains(got, "git activity") {
		t.Errorf("prompt missing git activity block:\n%s", got)
	}
	if !strings.Contains(got, "seed commit") {
		t.Errorf("prompt missing the seeded commit:\n%s", got)
	}
}
