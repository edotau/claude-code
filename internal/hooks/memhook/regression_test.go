package memhook

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
)

func TestFailedStopDispatchLeavesWorkForFinalHook(t *testing.T) {
	finalHooks := []struct {
		name string
		run  func(*core.Input) core.Result
	}{{triggerSessionEnd, End}, {triggerPreCompact, Compact}}
	for _, hook := range finalHooks {
		t.Run(hook.name, func(t *testing.T) {
			u := newUpdateRun(t)
			writeEdits(t, u.tr, 5)
			StubSpawnForTest(t, false)
			if got := u.stop(); got != core.ExitBlock {
				t.Fatalf("failed Stop dispatch code=%d, want block", got)
			}
			if st, ok := readStamp(stampPath("s1")); !ok || st.edits != 0 {
				t.Errorf("failed dispatch covered work: stamp=%+v exists=%v", st, ok)
			}
			calls := &StubSpawnForTest(t, true).Update
			in := agentIn(t, updatePayload(u.tr, u.cwd))
			hook.run(in)
			if calls.Load() != 1 {
				t.Fatalf("%s skipped retry after failed Stop: spawns=%d", hook.name, calls.Load())
			}
			if st, ok := readStamp(stampPath("s1")); !ok || st.state != "bg" || st.edits != 5 {
				t.Fatalf("successful retry stamp=%+v exists=%v, want bg covering 5 edits", st, ok)
			}
		})
	}
}

func TestFailedWorkerMessageUsesWorkUnits(t *testing.T) {
	u := newUpdateRun(t)
	writeUserTurns(t, u.tr, 6)
	StubSpawnForTest(t, false)
	for range 2 {
		var note bytes.Buffer
		code := sessionUpdateCode(agentIn(t, updatePayload(u.tr, u.cwd)), &note)
		if code != core.ExitBlock || !strings.Contains(note.String(), "~3 work units this session") {
			t.Fatalf("six user turns and zero edits: code=%d message=%q, want block with 3 work units", code, note.String())
		}
		if strings.Contains(note.String(), "file edits") {
			t.Fatalf("work units mislabeled as file edits: %q", note.String())
		}
	}
}

func TestIdleUpdateHoldsSessionDispatchLock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 5)
	previous := spawnUpdate
	spawnUpdate = func(string, string, string) bool {
		ran, err := paths.TryFileLock(stampPath("locked-idle")+".lock", 0, func() error { return nil })
		if err != nil {
			t.Errorf("probe session dispatch lock: %v", err)
		}
		if ran {
			t.Error("idle dispatch ran without holding the session stamp lock")
		}
		return true
	}
	t.Cleanup(func() { spawnUpdate = previous })
	dispatched, failed := idleUpdateOnce(tr, filepath.Dir(tr), "locked-idle", io.Discard)
	if !dispatched || failed {
		t.Fatalf("idle dispatch=%v failed=%v", dispatched, failed)
	}
	if st, ok := readStamp(stampPath("locked-idle")); !ok || st.edits != 5 {
		t.Fatalf("idle dispatch did not persist coverage: %+v exists=%v", st, ok)
	}
}

func statErrorTranscript(t *testing.T) string {
	t.Helper()
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.Symlink(tr, tr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tr); err == nil || os.IsNotExist(err) {
		t.Fatalf("fixture must produce a non-not-exist stat error: %v", err)
	}
	return tr
}

func TestWatcherRetriesStatErrors(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := statErrorTranscript(t)
	w := watchLoop{tr: tr, cwd: filepath.Dir(tr), session: "retry-stat", idle: time.Second, stderr: io.Discard}
	if w.poll() {
		t.Fatal("watcher exited on a retryable stat error")
	}
	if err := os.Remove(tr); err != nil {
		t.Fatal(err)
	}
	writeEdits(t, tr, 5)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tr, old, old); err != nil {
		t.Fatal(err)
	}
	calls := &StubSpawnForTest(t, true).Update
	if w.poll() || calls.Load() != 1 {
		t.Fatalf("watcher did not recover and dispatch after stat error: spawns=%d", calls.Load())
	}
	if err := os.Remove(tr); err != nil {
		t.Fatal(err)
	}
	if !w.poll() {
		t.Fatal("watcher must exit when transcript actually disappears")
	}
}
