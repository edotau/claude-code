package hooks

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/worktree"
)

func TestLaneMergeVerdict(t *testing.T) {
	for report, want := range map[string]string{
		"done\nUNFIXED: none":         "none",
		"done\n**UNFIXED: none.**":    "none",
		"done":                        "missing",
		"UNFIXED:":                    "missing",
		"UNFIXED: a flaky test; lint": "a flaky test; lint",
	} {
		if got := laneVerdict(report); got != want {
			t.Errorf("laneVerdict(%q) = %q, want %q", report, got, want)
		}
	}
}

func TestLaneMergeLeafReportReadsHandback(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "agent-a.jsonl")
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t","name":"SubagentHandback",` +
		`"input":{"message":"ok\nUNFIXED: none"}}]}}` + "\n"
	if err := os.WriteFile(tr, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := laneVerdict(leafReport(tr)); got != "none" {
		t.Fatalf("verdict from the hand-back = %q, want none", got)
	}
}

func TestLaneMergeTranscriptStampMovesWhenLeafRunsAgain(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "agent-a.jsonl")
	if err := os.WriteFile(tr, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := transcriptStamp(tr)
	f, err := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("b\n")
	f.Close()
	if transcriptStamp(tr) == seen {
		t.Fatal("stamp unchanged after the leaf wrote again")
	}
}

func TestLaneMergeRestrictionFiles(t *testing.T) {
	got := restrictionFiles([]string{"internal/hooks/safety/safety.go", "internal/settings/hookspec.go",
		"settings.json", "README.md", "internal/hooks/lanemerge.go", "(diff unreadable)"})
	want := []string{"internal/hooks/safety/safety.go", "internal/settings/hookspec.go", "settings.json", "(diff unreadable)"}
	if !slices.Equal(got, want) {
		t.Fatalf("restrictionFiles = %v, want %v", got, want)
	}
}

const firstQueued, laterQueued = 100, 200

func mustQueue(t *testing.T, q worktree.QueuedLane) {
	t.Helper()
	if err := worktree.Enqueue(q); err != nil {
		t.Fatal(err)
	}
}

// Enqueue twice keeps one entry and its first time; Dequeue twice leaves no file.
func TestLaneMergeQueueIdempotent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	main, lane := t.TempDir(), t.TempDir()
	mustQueue(t, worktree.QueuedLane{Lane: lane, Repo: main, Reason: "uncommitted work", QueuedAt: firstQueued})
	mustQueue(t, worktree.QueuedLane{Lane: lane, Repo: main, Reason: "unfixed items", QueuedAt: laterQueued})
	got := worktree.Queued(main)
	if len(got) != 1 || got[0].QueuedAt != firstQueued || got[0].Reason != "unfixed items" {
		t.Fatalf("queue after two enqueues = %+v", got)
	}
	for range 2 {
		if err := worktree.Dequeue(lane); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(worktree.MergeQueueFile()); !os.IsNotExist(err) {
		t.Fatalf("queue file after dequeue: %v", err)
	}
}

func TestLaneMergeQueueDropsRemovedLane(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	main, lane := t.TempDir(), t.TempDir()
	mustQueue(t, worktree.QueuedLane{Lane: lane, Repo: main, Reason: "uncommitted work"})
	if err := os.Remove(lane); err != nil {
		t.Fatal(err)
	}
	if got := worktree.Queued(main); len(got) != 0 {
		t.Fatalf("a removed lane stays queued: %+v", got)
	}
}

func TestJobDoneReloadsRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "p"}, time.Now())
	rec.ID, rec.PID = "job-reload", os.Getpid()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if jobFinished(rec) {
		t.Fatal("live PID reported finished")
	}
	stale := rec
	rec.PID = 0
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if !jobFinished(stale) {
		t.Fatal("PID 0 reported unfinished")
	}
}
