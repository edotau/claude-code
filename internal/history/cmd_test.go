package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/paths"
)

// TestHistorySnapshotFailureDoesNotReportUnchanged: a failed snapshot exits 1 and prints nothing on stdout.
func TestHistorySnapshotFailureDoesNotReportUnchanged(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(paths.CacheDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	// A regular file where the history repo directory belongs makes ensureRepo fail.
	if err := os.WriteFile(filepath.Join(paths.CacheDir(), "history"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int { return historySnapshot(nil) })
	if code != 1 {
		t.Fatalf("failed snapshot exit = %d, want 1; stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "unchanged") {
		t.Errorf("failed snapshot printed %q on stdout", stdout)
	}
}

// TestHistoryLogJSONKeepsConfigPlaneShape: `history log --format json` emits {"config": [...]} on an empty history.
func TestHistoryLogJSONKeepsConfigPlaneShape(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	_, stdout, _ := hosttest.CaptureOutput(t, func() int { return historyLog([]string{"--format", "json"}) })
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%q", err, stdout)
	}
	if _, ok := got["config"]; len(got) != 1 || !ok {
		t.Errorf("history log JSON keys = %v, want only \"config\"", got)
	}
}

// TestHistoryHelpNamesConfigOnly: the history description must not claim memory coverage.
func TestHistoryHelpNamesConfigOnly(t *testing.T) {
	if !strings.Contains(historyPage.About, "config") || strings.Contains(historyPage.About, "memory") {
		t.Errorf("history description = %q, must name config only", historyPage.About)
	}
}

// TestHistoryLogRejectsNegativeCount: -n -1 is a usage error (exit 2), not a leaked `git log --1` failure.
func TestHistoryLogRejectsNegativeCount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := hosttest.CaptureOutput(t, func() int { return historyLog([]string{"-n", "-1"}) })
	if code != 2 || !strings.Contains(stderr, "-n must be") {
		t.Errorf("code=%d stderr=%q, want exit 2 naming -n", code, stderr)
	}
}
