package hooks

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

func stubWatcher(t *testing.T, ok bool) *[][]string {
	t.Helper()
	var calls [][]string
	orig := spawnWatcher
	spawnWatcher = func(args []string, _ string) bool { calls = append(calls, args); return ok }
	t.Cleanup(func() { spawnWatcher = orig })
	return &calls
}

// TestNewestActivityNoSubagentsDirReturnsMainMtime: with no subagents dir at all, the main transcript's
// own mtime is the newest activity — there's nothing else to compare it against.
func TestNewestActivityNoSubagentsDirReturnsMainMtime(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tr, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainMtime := time.Now().Add(-time.Hour)
	if got := newestActivity(tr, mainMtime); !got.Equal(mainMtime) {
		t.Fatalf("newestActivity with no subagents dir = %v, want the main mtime %v", got, mainMtime)
	}
}

// TestNewestActivityNewerSubagentFileWins: a subagent transcript that mtimed AFTER the main transcript
// must make the watcher re-evaluate — the session isn't idle just because the parent stopped appending.
func TestNewestActivityNewerSubagentFileWins(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tr, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(subDir, "agent-a.jsonl")
	if err := os.WriteFile(agent, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainMtime := time.Now().Add(-time.Hour)
	newer := time.Now()
	if err := os.Chtimes(agent, newer, newer); err != nil {
		t.Fatal(err)
	}
	got := newestActivity(tr, mainMtime)
	if !got.After(mainMtime) {
		t.Fatalf("newestActivity = %v, want it to pick up the newer subagent file's mtime (after %v)", got, mainMtime)
	}
}

// A quiet transcript with uncovered work dispatches once; covered work stays quiet.
func TestIdleHarvestOnceDispatchesUncoveredWork(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 9)
	calls := stubSpawn(t, true)
	writeStamp(stampPath("idle"), time.Now().Add(-time.Hour), "bg", 4) // a checkpoint covered 4 of the 9

	if !idleHarvestOnce(tr, filepath.Dir(tr), "idle", io.Discard) || calls.Load() != 1 {
		t.Fatalf("5 uncovered edits on a quiet transcript must dispatch the final harvest (calls=%d)", calls.Load())
	}
	if st, _ := readStamp(stampPath("idle")); !st.bg || st.edits != 9 {
		t.Errorf("stamp after the idle harvest = %+v, want bg covering all 9 edits", st)
	}
	if idleHarvestOnce(tr, filepath.Dir(tr), "idle", io.Discard) || calls.Load() != 1 {
		t.Error("a second pass with everything covered must not dispatch again")
	}
	writeEdits(t, tr, 2)
	_ = os.Remove(stampPath("idle"))
	if idleHarvestOnce(tr, filepath.Dir(tr), "idle", io.Discard) {
		t.Error("under the edit floor must not dispatch")
	}
	var note bytes.Buffer
	writeEdits(t, tr, 5)
	stubSpawn(t, false)
	if idleHarvestOnce(tr, filepath.Dir(tr), "idle2", &note); !strings.Contains(note.String(), "not spawned") {
		t.Errorf("a failed spawn must be noted: %q", note.String())
	}
}

// TestIdleHarvestOnceCountsSubagentEdits: the idle watcher's edit count is the same countEdits unit the
// Stop gate's stamp uses — main transcript edits PLUS any dispatched subagent's own edits — so growth in
// either one past the covered total still dispatches the final harvest.
func TestIdleHarvestOnceCountsSubagentEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 1)
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEdits(t, filepath.Join(subDir, "agent-a.jsonl"), 10)
	calls := stubSpawn(t, true)

	if !idleHarvestOnce(tr, filepath.Dir(tr), "idle-sub", io.Discard) || calls.Load() != 1 {
		t.Fatalf("1 main + 10 subagent edits must dispatch the final harvest (calls=%d)", calls.Load())
	}
	if st, _ := readStamp(stampPath("idle-sub")); !st.bg || st.edits != 11 {
		t.Errorf("stamp after the idle harvest = %+v, want bg covering 11 edits", st)
	}

	writeEdits(t, tr, 7) // 6 more main edits (1 -> 7); the subagent file is untouched
	if !idleHarvestOnce(tr, filepath.Dir(tr), "idle-sub", io.Discard) || calls.Load() != 2 {
		t.Fatalf("6 more main edits past the covered 11 must dispatch again (calls=%d)", calls.Load())
	}
}

// The Stop gate claims one watcher per session at its first dispatch — never a second.
func TestSessionHarvestSpawnsOneIdleWatcherPerSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.HarvestChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	cwd := t.TempDir()
	writeEdits(t, tr, 6)
	stubSpawn(t, true)
	watches := stubWatcher(t, true)
	run := func() { sessionHarvest(ParseInput(strings.NewReader(harvestPayload(tr, cwd))), &bytes.Buffer{}) }

	run()
	writeStamp(stampPath("s1"), time.Now().Add(-time.Hour), "bg", 6)
	writeEdits(t, tr, 20) // a re-arm dispatch of the same session
	run()
	if len(*watches) != 1 {
		t.Fatalf("watcher spawned %d times, want exactly once: %v", len(*watches), *watches)
	}
	if got := strings.Join((*watches)[0], " "); !strings.HasPrefix(got, "--session s1 --transcript "+tr+" --cwd "+cwd+" --owner ") {
		t.Errorf("watcher argv = %q", got)
	}
	if _, err := os.Stat(filepath.Join(memory.StateDir(), "watch-s1")); err != nil {
		t.Error("the per-session claim file must exist after the spawn")
	}

	// A failed spawn releases the claim, so the next harvest dispatch (a re-arm) tries again.
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	failed := stubWatcher(t, false)
	run()
	writeStamp(stampPath("s1"), time.Now().Add(-time.Hour), "bg", 6)
	writeEdits(t, tr, 20)
	run()
	if len(*failed) != 2 {
		t.Errorf("a failed watcher spawn must be retried on the next dispatch (got %d)", len(*failed))
	}
	if _, err := os.Stat(filepath.Join(memory.StateDir(), "watch-s1")); err == nil {
		t.Error("a failed spawn must release its claim file")
	}
}

// The watcher is bound to its session process: once that pid is gone it exits on the next poll.
func TestIdleHarvestWatchExitsWhenOwnerDies(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 6)
	prev := harvestWatchPoll
	harvestWatchPoll = 5 * time.Millisecond
	t.Cleanup(func() { harvestWatchPoll = prev })
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run true:", err)
	}
	done := make(chan int, 1)
	go func() {
		done <- IdleHarvestWatch([]string{"--session", "own", "--transcript", tr, "--cwd", filepath.Dir(tr),
			"--owner", strconv.Itoa(cmd.Process.Pid)}, io.Discard)
	}()
	select {
	case rc := <-done:
		if rc != 0 {
			t.Errorf("watch exit = %d, want 0", rc)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exit after its owner died")
	}
}

// A quiet transcript is harvested from inside the loop; a vanished transcript ends the watch.
func TestIdleHarvestWatchHarvestsQuietTranscript(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 5)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(tr, old, old)
	prev := harvestWatchPoll
	harvestWatchPoll = 5 * time.Millisecond
	t.Cleanup(func() { harvestWatchPoll = prev })
	calls := stubSpawn(t, true)
	var note bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- IdleHarvestWatch([]string{"--session", "quiet", "--transcript", tr, "--cwd", filepath.Dir(tr), "--idle", "1s"}, &note)
	}()
	for i := 0; i < 200 && calls.Load() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	_ = os.Remove(tr)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exit after the transcript vanished")
	}
	if calls.Load() != 1 || !strings.Contains(note.String(), "final harvest dispatched") {
		t.Errorf("calls=%d note=%q", calls.Load(), note.String())
	}
}

// TestIdleHarvestWatchSkipsGlobWhileMainTranscriptFresh: while the main transcript's own mtime is
// within the idle window, the loop must not even reach newestActivity's subagent glob or dispatch —
// a subagent transcript with plenty of uncovered edits must not trigger a harvest during that window.
func TestIdleHarvestWatchSkipsGlobWhileMainTranscriptFresh(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 6) // sets the main transcript's mtime to "now" as a side effect of the write
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(subDir, "agent-a.jsonl")
	writeEdits(t, agent, 9)
	prev := harvestWatchPoll
	harvestWatchPoll = 5 * time.Millisecond
	t.Cleanup(func() { harvestWatchPoll = prev })
	calls := stubSpawn(t, true)
	done := make(chan int, 1)
	go func() {
		done <- IdleHarvestWatch([]string{"--session", "fresh", "--transcript", tr, "--cwd", filepath.Dir(tr),
			"--idle", "200ms"}, io.Discard)
	}()
	time.Sleep(60 * time.Millisecond) // several polls, well inside the 200ms idle window
	if n := calls.Load(); n != 0 {
		t.Errorf("dispatch fired while the main transcript was still fresh: calls=%d", n)
	}
	_ = os.Remove(tr)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not exit after the transcript vanished")
	}
}

func TestIdleHarvestWatchRefusesBadFlags(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 6)
	var errb bytes.Buffer
	if rc := IdleHarvestWatch([]string{"--session", "x", "--transcript", tr, "--cwd", filepath.Join(t.TempDir(), "gone")}, &errb); rc != 2 {
		t.Errorf("watch with a missing cwd = %d, want 2", rc)
	}
	if !strings.Contains(errb.String(), "not a directory") {
		t.Errorf("stderr = %q", errb.String())
	}
	if rc := IdleHarvestWatch([]string{"--transcript", tr, "--cwd", filepath.Dir(tr)}, io.Discard); rc != 2 {
		t.Errorf("watch without --session = %d, want 2", rc)
	}
}
