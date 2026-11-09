package memorycmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/memstore"
)

func TestMemoryReportRegistersFreshRepoOrigin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo := gittest.Repo(t)
	dir := memstore.Dir(repo)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("fresh repo store must not exist: %v", err)
	}
	input := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(input, []byte(`{"report":"# Review\n\nFresh repo report.\n","decisions":["Keep reports together."]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		checkFreshRepoReportPaths(t, repo, input, dir)
		if got := memstore.Dir(repo); got != dir {
			t.Fatalf("report split repo store: got %q, want %q", got, dir)
		}
	}
	entries, err := memstore.RepoDirs()
	if err != nil {
		t.Fatal(err)
	}
	entry := memstore.FindEntry(entries, memstore.Resolve(repo).Origin)
	if entry == nil || entry.Dir != dir {
		t.Fatalf("report store %q is not discoverable by RepoDirs: %v", dir, entries)
	}
}

func checkFreshRepoReportPaths(t *testing.T, repo, input, dir string) {
	t.Helper()
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Run([]string{"report", "--repo", repo, "--label", "origin-regression", "--file", input, "--format", "json"}, nil)
	})
	if code != 0 {
		t.Fatalf("memory report: exit=%d stderr=%q", code, stderr)
	}
	var out struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("memory report JSON: %v; stdout=%q", err, stdout)
	}
	if len(out.Paths) != 2 {
		t.Fatalf("memory report paths = %v, want report and decisions", out.Paths)
	}
	for _, path := range out.Paths {
		if filepath.Dir(path) != memstore.ReportsDir(dir) {
			t.Fatalf("report path %q is outside original store %q", path, dir)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("reported file %q: %v", path, err)
		}
	}
}
