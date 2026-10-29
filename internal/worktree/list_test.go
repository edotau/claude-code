package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func commitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestListEmptyRepo(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0: %+v", len(entries), entries)
	}
}

func TestListCleanMergedWorktree(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "clean", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	wantPath, err := canonical(dest)
	if err != nil {
		t.Fatal(err)
	}
	e := entries[0]
	if e.Path != wantPath {
		t.Fatalf("Path = %q, want %q", e.Path, wantPath)
	}
	if !e.Registered || !e.Exists {
		t.Fatalf("Registered/Exists = %v/%v, want true/true", e.Registered, e.Exists)
	}
	if e.Dirty {
		t.Error("want Dirty = false")
	}
	if !e.Merged {
		t.Error("want Merged = true (no new commits in the worktree)")
	}
	if e.Orphaned {
		t.Error("want Orphaned = false")
	}
	if len(e.Problems) != 0 {
		t.Errorf("unexpected Problems: %v", e.Problems)
	}
}

func TestListDirtyWorktree(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "dirty", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Dirty {
		t.Fatalf("entries = %+v, want one Dirty entry", entries)
	}
}

func TestListUnmergedWorktree(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "ahead", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "ahead of main")
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Merged {
		t.Error("want Merged = false for a worktree with unmerged commits")
	}
	if entries[0].Dirty {
		t.Error("an extra commit alone should not mark the tree Dirty")
	}
}

func TestListOrphanedDirectory(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	// A directory under Root that git never registered as a worktree.
	if err := os.MkdirAll(filepath.Join(repo.Root, "stray"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if !e.Orphaned {
		t.Error("want Orphaned = true for an unregistered directory")
	}
	if e.Registered {
		t.Error("want Registered = false")
	}
	// Inspect never runs on a non-worktree directory; a stray dir has no git identity to classify.
	if len(e.Problems) == 0 {
		t.Error("want Problems recorded for a directory with no usable git identity")
	}
}

func TestListLockedWorktree(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	if _, err := Create(repo, "locked", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	lockCmd := exec.Command("git", "-C", repo.Main, "worktree", "lock", filepath.Join(repo.Root, "locked"))
	lockCmd.Env = gitEnv()
	if out, err := lockCmd.CombinedOutput(); err != nil {
		t.Fatalf("worktree lock: %v: %s", err, out)
	}
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].Locked {
		t.Fatalf("entries = %+v, want one Locked entry", entries)
	}
}

func TestListStaleByReflogAge(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "old", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gdCmd := exec.Command("git", "-C", dest, "rev-parse", "--path-format=absolute", "--git-dir")
	gdCmd.Env = gitEnv()
	gitDirOut, err := gdCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	reflog := filepath.Join(strings.TrimSpace(string(gitDirOut)), "logs", "HEAD")
	old := time.Now().Add(-15 * 24 * time.Hour)
	if err := os.Chtimes(reflog, old, old); err != nil {
		t.Fatal(err)
	}
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if !entries[0].Stale {
		t.Error("want Stale = true for activity older than StaleAfter")
	}
	if entries[0].ActivitySource != "reflog" {
		t.Errorf("ActivitySource = %q, want reflog", entries[0].ActivitySource)
	}
}

func TestListDetachedHEADMain(t *testing.T) {
	t.Parallel()
	repo := testDetachedRepo(t)
	if _, err := Create(repo, "fromdetached", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	// Detached main's target is an OID (currentBranch falls back to rev-parse HEAD); merge-base
	// against it must still succeed since the worktree's HEAD descends from the same commit.
	if !entries[0].Merged {
		t.Errorf("want Merged = true against a detached-HEAD OID target, Problems=%v", entries[0].Problems)
	}
}

func TestListUnionsFilesystemAndRegistration(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	if _, err := Create(repo, "registered", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo.Root, "unregistered"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := List(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	var sawRegistered, sawUnregistered bool
	for _, e := range entries {
		switch filepath.Base(e.Path) {
		case "registered":
			sawRegistered = e.Registered && !e.Orphaned
		case "unregistered":
			sawUnregistered = !e.Registered && e.Orphaned
		}
	}
	if !sawRegistered || !sawUnregistered {
		t.Fatalf("entries = %+v, missing expected registered/unregistered pair", entries)
	}
}

func TestListPropagatesFilesystemReadError(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	if err := os.MkdirAll(filepath.Dir(repo.Root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repo.Root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := List(repo); err == nil {
		t.Fatal("List succeeded despite a non-directory worktree root")
	}
}
