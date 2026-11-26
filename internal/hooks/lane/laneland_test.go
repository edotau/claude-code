package lane

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory/memtest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
	"github.com/edotau/claude-code/internal/worktree/worktreetest"
)

func TestLaneLandVerdict(t *testing.T) {
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

func TestLaneLandLeafReportReadsHandback(t *testing.T) {
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

func TestLaneLandTranscriptStampMovesWhenLeafRunsAgain(t *testing.T) {
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

func TestLaneLandRestrictionFiles(t *testing.T) {
	got := restrictionFiles([]string{"internal/hooks/safety/safety.go", "internal/settings/hookspec.go",
		"settings.json", "README.md", "internal/hooks/laneland.go", "(diff unreadable)"})
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
func TestLaneLandQueueIdempotent(t *testing.T) {
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
	if _, err := os.Stat(worktree.LandQueueFile()); !os.IsNotExist(err) {
		t.Fatalf("queue file after dequeue: %v", err)
	}
}

func TestLaneLandQueueDropsRemovedLane(t *testing.T) {
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

func TestHandbackVerdictReadsTypedStatus(t *testing.T) {
	for h, want := range map[string]wire.Handback{
		"none":          {Status: "DONE", Summary: "s"},
		"BLOCKED":       {Status: "BLOCKED", Summary: "s"},
		"x; y":          {Status: "DONE", Unfixed: []string{"x", "y"}},
		"NEEDS_CONTEXT": {Status: "NEEDS_CONTEXT", Unfixed: []string{"none."}},
	} {
		if got := HandbackVerdict(&want); got != h {
			t.Errorf("HandbackVerdict(%+v) = %q, want %q", want, got, h)
		}
	}
}

func TestLaneQueueOncePerSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	main, a, b := t.TempDir(), t.TempDir(), t.TempDir()
	if out, err := exec.Command("git", "-C", main, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	main = paths.Canon(main)
	tip := int64(0)
	laneTipTime = func(string) int64 { return tip }
	t.Cleanup(func() { laneTipTime = func(string) int64 { return 0 } })
	mustQueue(t, worktree.QueuedLane{Lane: a, Repo: main, Reason: "uncommitted work", QueuedAt: firstQueued})
	mustQueue(t, worktree.QueuedLane{Lane: b, Repo: main, Reason: "unfixed items", QueuedAt: firstQueued})
	if got := laneQueueContext(main, "s1"); strings.Count(got, "land queue:") != 2 {
		t.Fatalf("first prompt = %q", got)
	}
	if got := laneQueueContext(main, "s1"); got != "" {
		t.Fatalf("unchanged queue printed again: %q", got)
	}
	mustQueue(t, worktree.QueuedLane{Lane: a, Repo: main, Reason: "changed reason", QueuedAt: firstQueued})
	if got := laneQueueContext(main, "s1"); !strings.Contains(got, "changed reason") || strings.Contains(got, "unfixed items") {
		t.Fatalf("changed entry = %q", got)
	}
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	if got := laneQueueContext(main, "s1"); !strings.Contains(got, "1 lanes waiting") || strings.Contains(got, filepath.Base(b)) {
		t.Fatalf("missing lane = %q", got)
	}
	tip = firstQueued + 1
	if got := laneQueueContext(main, "s1"); got != "" {
		t.Fatalf("moved tip kept its entry: %q", got)
	}
}

func TestLandRunsWithoutSerialLock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(laneStateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	inspect, land := inspectLane, runWorktreeLand
	t.Cleanup(func() { inspectLane, runWorktreeLand = inspect, land })
	inspectLane = func(laneJob, *worktree.Repo, string) (worktree.QueuedLane, bool, bool) {
		return worktree.QueuedLane{}, false, false
	}
	free := false
	runWorktreeLand = func(string, string, string) (int, string) {
		free, _ = paths.TryFileLock(filepath.Join(laneStateDir(), "serial.lock"), time.Millisecond, func() error { return nil })
		return 0, ""
	}
	landAttempt(laneJob{lane: t.TempDir()}, &worktree.Repo{Main: t.TempDir()}, "")
	if !free {
		t.Fatal("serial.lock still held while worktree land runs")
	}
}

func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	scan := worktree.ProcessScan
	worktreetest.StubProcessScanForTest(scan.In, scan.Up, scan.Procs, scan.Agents)
	tb := &hosttest.MainTB{}
	memtest.StubSpawn(tb, true)
	worktree.PeerScan = func(string, []string) ([]worktree.Proc, error) { return nil, nil }
	code := m.Run()
	for _, f := range tb.Cleanups {
		f()
	}
	hosttest.Cleanup()
	cleanup()
	os.Exit(code)
}
