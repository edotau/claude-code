package project

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncCheckReportsPruneReadError(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	if code := Sync(Options{Out: io.Discard}); code != 0 {
		t.Fatalf("initial sync = %d, want 0", code)
	}
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 0 {
		t.Fatalf("current projections check = %d, want 0", code)
	}
	// ENOTDIR exercises the read-error path even when the test runs with permission bypass privileges.
	legacy := LegacyGeminiSkillsDir(home)
	projectWriteFile(t, legacy, "not a directory\n")
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 1 {
		t.Fatalf("check with unreadable prune destination = %d, want 1", code)
	}
	if err := os.Remove(legacy); err != nil {
		t.Fatal(err)
	}
	if code := Sync(Options{Check: true, Out: io.Discard}); code != 0 {
		t.Errorf("check with missing optional prune destination = %d, want 0", code)
	}
}

func assertPruneResult(t *testing.T, got, want syncResult) {
	t.Helper()
	if got != want {
		t.Fatalf("prune result = %+v, want %+v", got, want)
	}
}

func TestLegacySkillPrunePreservesMarkerForRetry(t *testing.T) {
	for _, extra := range []string{"notes.txt", "subdir/notes.txt"} {
		t.Run(extra, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "legacy")
			marker := filepath.Join(dir, "SKILL.md")
			content := realSkillBannerPrefix + "legacy/SKILL.md; do not edit by hand. -->\n"
			projectWriteFile(t, marker, content)
			projectWriteFile(t, filepath.Join(dir, extra), "hand-authored\n")
			res := pruneLegacyGeminiSkills(io.Discard, root, false)
			assertPruneResult(t, res, syncResult{errored: 1})
			got, err := os.ReadFile(marker)
			if err != nil || string(got) != content {
				t.Fatalf("blocked prune destroyed retry marker: content %q, err %v", got, err)
			}
			entry, _, _ := strings.Cut(extra, "/")
			if err := os.RemoveAll(filepath.Join(dir, entry)); err != nil {
				t.Fatal(err)
			}
			res = pruneLegacyGeminiSkills(io.Discard, root, false)
			assertPruneResult(t, res, syncResult{pruned: 1})
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Errorf("legacy directory survived successful retry: %v", err)
			}
		})
	}
}
