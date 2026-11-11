package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestListKeepsDotDotPrefixedChildRegistered(t *testing.T) {
	repo := testRepo(t)
	wt, err := Create(repo, "..scratch", CreateOptions{Branch: "scratch"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != wt || !entries[0].Registered || entries[0].Orphaned {
		t.Fatalf("safe dot-dot-prefixed child lost its registration: %+v", entries)
	}
	results, err := CleanAll(repo, false)
	if err != nil || len(results) != 1 || results[0].Path != wt {
		t.Fatalf("CleanAll skipped registered child: results=%+v err=%v", results, err)
	}
}

func TestUnionFilesystemPropagatesRegisteredStatError(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "loop")
	if err := os.Symlink("loop", wt); err != nil {
		t.Fatal(err)
	}
	entries, err := unionFilesystem(&Repo{Root: root}, []Entry{{Path: wt, Registered: true}})
	if !errors.Is(err, syscall.ELOOP) || !strings.Contains(err.Error(), "stat registered worktree") {
		t.Fatalf("registered stat error became a missing entry: entries=%+v err=%v", entries, err)
	}
}

func TestActivityChoosesNewerIndex(t *testing.T) {
	gitDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleAfter).Truncate(time.Second)
	recent := old.Add(staleAfter)
	for file, stamp := range map[string]time.Time{"logs/HEAD": old, "index": recent} {
		path := filepath.Join(gitDir, filepath.FromSlash(file))
		regressionWriteFile(t, path, "activity\n")
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	got, source := activity(gitDir, t.TempDir())
	if !got.Equal(recent) || source != "index" {
		t.Fatalf("newer index ignored: time=%v source=%q; want %v, index", got, source, recent)
	}
}

func TestListCleanEligibilityReflectsLiveRefusals(t *testing.T) {
	t.Run("in-use", func(t *testing.T) {
		stubProcesses(t, func(string) ([]Proc, error) {
			return []Proc{{PID: 999999, Comm: "busy-agent"}}, nil
		})
		entry := regressionListEntry(t, func(string) {})
		if entry.Busy == "" || entry.EligibleForClean() {
			t.Fatalf("in-use worktree advertised for clean: %+v", entry)
		}
	})
	t.Run("operation", func(t *testing.T) {
		entry := regressionListEntry(t, func(wt string) {
			gitDir, err := gitDirFromPath(wt)
			if err != nil {
				t.Fatal(err)
			}
			regressionWriteFile(t, filepath.Join(gitDir, "MERGE_HEAD"), headOf(wt)+"\n")
		})
		if !strings.Contains(entry.Busy, "in progress") || entry.EligibleForClean() {
			t.Fatalf("mid-merge worktree advertised for clean: %+v", entry)
		}
	})
	t.Run("inspection", func(t *testing.T) {
		entry := Entry{Exists: true, Merged: true, Problems: []string{"status inspection failed"}}
		if entry.EligibleForClean() {
			t.Fatal("inspection failures must block eligibility even when other clean flags pass")
		}
		entry.Problems = nil
		if !entry.EligibleForClean() {
			t.Fatal("clean, unoccupied worktree must remain eligible")
		}
	})
}

func regressionListEntry(t *testing.T, prepare func(string)) Entry {
	t.Helper()
	repo := testRepo(t)
	wt, err := Create(repo, "feature", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prepare(wt)
	entries, err := List(repo)
	if err != nil || len(entries) != 1 {
		t.Fatalf("List: entries=%+v err=%v", entries, err)
	}
	return entries[0]
}
