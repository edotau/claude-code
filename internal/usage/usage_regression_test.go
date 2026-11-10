package usage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewestModTimePropagatesInfoError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "transcript.jsonl"), "{}")
	if err := os.Chmod(dir, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("fixture must allow ReadDir before Info fails: %v", err)
	}
	if _, err := entries[0].Info(); !errors.Is(err, os.ErrPermission) {
		t.Skipf("filesystem does not enforce directory search permissions: %v", err)
	}
	newest, err := newestModTime(dir)
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "walk "+dir) {
		t.Fatalf("DirEntry.Info error was swallowed: newest=%v err=%v", newest, err)
	}
}

type partialReportWriter struct {
	writes int
	err    error
}

func (w *partialReportWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		return len(p), nil
	}
	return 0, w.err
}

func isolatedUsageDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, env := range []string{"CLAUDE_CONFIG_DIR", "HOME", "CODEX_HOME", "COPILOT_HOME", "XDG_DATA_HOME"} {
		t.Setenv(env, dir)
	}
	t.Setenv("OPENCODE_DB", ":memory:")
	return dir
}

func TestUsageTextReportsWriteFailure(t *testing.T) {
	dir := isolatedUsageDir(t)
	writeUsageFixture(t, dir, "project", "session", time.Minute,
		usageRecordLine("model", 7, 3, recentTS(time.Minute)))
	w := &partialReportWriter{err: errors.New("report destination closed")}
	var errw bytes.Buffer
	code := Run([]string{"--session", "session", "--providers=false"}, dir, w, &errw)
	if code != 1 || !strings.Contains(errw.String(), "usage: write output: report destination closed") {
		t.Fatalf("partial report exited successfully or hid write error: exit=%d stderr=%s", code, &errw)
	}
}
