package memory

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestIdleUpdateHoldsSessionDispatchLock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 5)
	previous := spawn.update
	spawn.update = func(string, string, string) bool {
		ran, err := paths.TryFileLock(stampPath("locked-idle")+".lock", 0, func() error { return nil })
		if err != nil {
			t.Errorf("probe session dispatch lock: %v", err)
		}
		if ran {
			t.Error("idle dispatch ran without holding the session stamp lock")
		}
		return true
	}
	t.Cleanup(func() { spawn.update = previous })
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
	calls := &stubSpawn(t, true).Update
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
