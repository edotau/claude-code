package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// A linked worktree's bank tag is its main checkout's name, never the branch dir it was checked out into.
func TestRepoSlugUsesMainCheckoutForLinkedWorktree(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := filepath.Join(t.TempDir(), "workspaces", "my-site", "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ptr := "gitdir: /Users/x/src/github.com/me/my-site/.git/worktrees/23710-abc\n"
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte(ptr), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RepoSlug(root); got != "my-site" {
		t.Errorf("RepoSlug(worktree) = %q, want my-site", got)
	}

	primary := filepath.Join(t.TempDir(), "Other.Repo")
	if err := os.MkdirAll(filepath.Join(primary, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := RepoSlug(primary); got != "other.repo" {
		t.Errorf("RepoSlug(primary) = %q, want other.repo", got)
	}

	rel := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, ".git"), []byte("gitdir: ../repo-b/.git/worktrees/wt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RepoSlug(rel); got != "repo-b" {
		t.Errorf("RepoSlug(relative gitdir) = %q, want repo-b", got)
	}
}

func TestRepoSlugConfigWorktree(t *testing.T) {
	main := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", main)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+main+"/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RepoSlug(root); got != "" {
		t.Errorf("RepoSlug(config worktree) = %q, want empty", got)
	}
}
