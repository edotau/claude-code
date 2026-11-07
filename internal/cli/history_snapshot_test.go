package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	code, stdout, stderr := captureOutput(t, func() int { return historySnapshot(nil) })
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
	_, stdout, _ := captureOutput(t, func() int { return historyLog([]string{"--format", "json"}) })
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%q", err, stdout)
	}
	if _, ok := got["config"]; len(got) != 1 || !ok {
		t.Errorf("history log JSON keys = %v, want only \"config\"", got)
	}
}

// TestHistoryMenuSummaryNamesConfigOnly: the history menu row must not claim memory coverage.
func TestHistoryMenuSummaryNamesConfigOnly(t *testing.T) {
	c := lookup("history")
	if c == nil || strings.Contains(c.summary, "memory") {
		t.Errorf("history summary = %+v, must not mention memory", c)
	}
}

// TestHistoryLogRejectsNegativeCount: -n -1 is a usage error (exit 2), not a leaked `git log --1` failure.
func TestHistoryLogRejectsNegativeCount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	code, _, stderr := captureOutput(t, func() int { return historyLog([]string{"-n", "-1"}) })
	if code != 2 || !strings.Contains(stderr, "-n must be") {
		t.Errorf("code=%d stderr=%q, want exit 2 naming -n", code, stderr)
	}
}
