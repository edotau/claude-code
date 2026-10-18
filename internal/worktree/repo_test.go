package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo makes a fresh git repo with one commit, env wired so every git invocation has an identity.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "--allow-empty", "-q", "-m", "init")
	dir, err := canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveRepoFromMainSubdirAndLinkedWorktree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main := gitRepo(t)

	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatalf("ResolveRepo(main): %v", err)
	}
	if repo.Main != main {
		t.Fatalf("Main = %q, want %q", repo.Main, main)
	}
	canon, _ := filepath.EvalSymlinks(home) // WorktreeRoot canonicalizes HOME
	wantRoot := filepath.Join(canon, "worktrees", filepath.Base(main))
	if repo.Root != wantRoot {
		t.Fatalf("Root = %q, want %q", repo.Root, wantRoot)
	}

	// From a subdirectory of main, identity must still resolve to main.
	sub := filepath.Join(main, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if repo2, err := ResolveRepo(sub); err != nil || repo2.Main != main {
		t.Fatalf("ResolveRepo(sub) = %+v, %v", repo2, err)
	}

	// From inside a linked worktree, identity must also resolve to the same main.
	linked, err := Create(repo, "linked", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if repo3, err := ResolveRepo(linked); err != nil || repo3.Main != main {
		t.Fatalf("ResolveRepo(linked) = %+v, %v", repo3, err)
	}
}

func TestResolveRepoNonGitDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	notRepo := t.TempDir()
	if _, err := ResolveRepo(notRepo); err == nil {
		t.Fatal("want error for non-git directory")
	}
}

func TestResolveWorktreeRejectsTraversalAndBadNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", "a/b", "a\\b", "../escape"} {
		if _, err := ResolveWorktree(repo, name); err == nil {
			t.Errorf("name %q: want error, got nil", name)
		}
	}
}

func TestResolveWorktreeAcceptsUnusualButSafeNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"has space", "dots.and.more", "unicode-éé", "UPPER-case_123"} {
		dest, err := ResolveWorktree(repo, name)
		if err != nil {
			t.Errorf("name %q: unexpected error %v", name, err)
			continue
		}
		if filepath.Dir(dest) != repo.Root {
			t.Errorf("name %q: dest %q not under root %q", name, dest, repo.Root)
		}
	}
}

func TestResolveWorktreeForeignBasenameCollision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	// A directory already occupying the destination that is not a worktree of this repo at all.
	foreign := filepath.Join(repo.Root, "taken")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWorktree(repo, "taken"); err == nil {
		t.Fatal("want collision error for a non-worktree directory occupying the name")
	}
}

func TestResolveWorktreeForeignRepoCollision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	// A directory occupying the destination that IS a git repo, but not a worktree of repo.Main.
	other := gitRepo(t)
	dest := filepath.Join(repo.Root, "other")
	if err := os.MkdirAll(repo.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWorktree(repo, "other"); err == nil {
		t.Fatal("want collision error for a foreign repo occupying the name")
	}
}

func TestResolveWorktreeSymlinkAtDestination(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	dest := filepath.Join(repo.Root, "link")
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveWorktree(repo, "link"); err == nil {
		t.Fatal("want error for a symlink at the destination")
	}
}

func TestResolveWorktreeResumesExisting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := Create(repo, "resume", "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := ResolveWorktree(repo, "resume")
	if err != nil {
		t.Fatalf("ResolveWorktree on existing worktree: %v", err)
	}
	if again != dest {
		t.Fatalf("got %q, want %q", again, dest)
	}
}

func TestResolveRepoMissingPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := ResolveRepo(missing); err == nil {
		t.Fatal("want error for a missing path")
	}
}

func TestRootUsesMainBasename(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := Root("/some/path/myrepo")
	home, _ = filepath.EvalSymlinks(home) // WorktreeRoot canonicalizes HOME
	want := filepath.Join(home, "worktrees", "myrepo")
	if got != want {
		t.Fatalf("Root = %q, want %q", got, want)
	}
}

func TestWorktreeRootUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	home, _ = filepath.EvalSymlinks(home) // WorktreeRoot canonicalizes HOME
	if got, want := WorktreeRoot(), filepath.Join(home, "worktrees"); got != want {
		t.Fatalf("WorktreeRoot = %q, want %q", got, want)
	}
}

// detachedHead is a minimal repo whose main checkout is in detached-HEAD state, for Phase 1's
// "detached HEAD" coverage requirement shared by list/lifecycle tests in this package.
func detachedHead(t *testing.T) string {
	t.Helper()
	dir := gitRepo(t)
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if out2, err := exec.Command("git", "-C", dir, "checkout", "-q", strings.TrimSpace(string(out))).CombinedOutput(); err != nil {
		t.Fatalf("checkout detached: %v: %s", err, out2)
	}
	return dir
}

func TestResolveRepoDetachedHEAD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := detachedHead(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatalf("ResolveRepo on detached HEAD main: %v", err)
	}
	if repo.Main != main {
		t.Fatalf("Main = %q, want %q", repo.Main, main)
	}
}
