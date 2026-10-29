package worktree

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var (
	repoSeq    atomic.Int64
	tmplOnce   sync.Once
	tmplDir    string
	tmplDirErr error
)

// templateRepo builds one init-committed repo per test binary; gitRepo copies it instead of spawning git twice.
func templateRepo(t *testing.T) string {
	t.Helper()
	tmplOnce.Do(func() {
		tmplDir, tmplDirErr = os.MkdirTemp("", "worktree-tmpl-")
		if tmplDirErr != nil {
			return
		}
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "--allow-empty", "-q", "-m", "init"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = tmplDir
			cmd.Env = gitEnv("GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
			if out, err := cmd.CombinedOutput(); err != nil {
				tmplDirErr = fmt.Errorf("git %v: %v: %s", args, err, out)
				return
			}
		}
	})
	if tmplDirErr != nil {
		t.Fatal(tmplDirErr)
	}
	return tmplDir
}

// copyTree copies src into dst preserving modes; symlinks are not expected in a fresh repo.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// gitRepo makes a fresh git repo with one commit, env wired so every git invocation has an identity.
func gitRepo(t *testing.T) string {
	t.Helper()
	// A globally unique basename: lease/operation slugs key on it, and every TempDir child is "001".
	dir := filepath.Join(t.TempDir(), fmt.Sprintf("repo%d", repoSeq.Add(1)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(templateRepo(t), dir); err != nil {
		t.Fatal(err)
	}
	dir, err := canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// testRepo builds an isolated Repo with its own worktree root without mutating HOME.
func testRepo(t *testing.T) *Repo {
	t.Helper()
	main := gitRepo(t)
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Repo{Main: main, Root: filepath.Join(root, "worktrees", filepath.Base(main))}
}

func testDetachedRepo(t *testing.T) *Repo {
	t.Helper()
	main := detachedHead(t)
	root, err := canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Repo{Main: main, Root: filepath.Join(root, "worktrees", filepath.Base(main))}
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
	linked, err := Create(repo, "linked", CreateOptions{})
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
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(notRepo))
	if _, err := ResolveRepo(notRepo); err == nil {
		t.Fatal("want error for non-git directory")
	}
}

func TestResolveWorktreeRejectsTraversalAndBadNames(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	for _, name := range []string{"", ".", "..", "a/b", "a\\b", "../escape"} {
		if _, err := ResolveWorktree(repo, name); err == nil {
			t.Errorf("name %q: want error, got nil", name)
		}
	}
}

func TestResolveWorktreeAcceptsUnusualButSafeNames(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
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
	t.Parallel()
	repo := testRepo(t)
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
	t.Parallel()
	repo := testRepo(t)
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
	t.Parallel()
	repo := testRepo(t)
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
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "resume", CreateOptions{})
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
	revCmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	revCmd.Env = gitEnv()
	out, err := revCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	coCmd := exec.Command("git", "-C", dir, "checkout", "-q", strings.TrimSpace(string(out)))
	coCmd.Env = gitEnv()
	if out2, err := coCmd.CombinedOutput(); err != nil {
		t.Fatalf("checkout detached: %v: %s", err, out2)
	}
	return dir
}

// TestResolveWorktreeRejectsLiveCheckoutSubdirUnderSymlinkedRoot is the regression test for Appendix A
// codex-review finding #4 (internal/worktree/repo.go:85): when a worktree root is symlinked straight
// onto the live main checkout (as `~/worktrees/.claude` -> `~/.claude` would be), a name like "internal"
// that already exists as a real subdirectory of the live checkout must never be accepted as if it were
// a worktree of that repo — common-dir equality alone is satisfied (it IS the same repo), but it is the
// live checkout's own source tree, not a worktree.
func TestResolveWorktreeRejectsLiveCheckoutSubdirUnderSymlinkedRoot(t *testing.T) {
	t.Parallel()
	main := gitRepo(t)
	// repo.Root pointed directly at the main checkout — the symlinked-root scenario, reproduced
	// without needing a real symlink: Root(main) == main is exactly what that symlink collapses to.
	repo := &Repo{Main: main, Root: main}

	internal := filepath.Join(main, "internal")
	if err := os.MkdirAll(internal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(internal, "source.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ResolveWorktree(repo, "internal"); err == nil {
		t.Fatal("ResolveWorktree: want an error — the live checkout's own subdirectory must never be treated as a worktree")
	}
}

// TestResolveWorktreeRejectsLiveCheckoutRootForNewDestination covers rejectLiveCheckoutRoot's own
// reason for existing unconditionally (internal/worktree/repo.go:90): a brand-new name that does NOT
// yet exist on disk under a worktree root symlinked into the live main checkout must still be
// refused, before Create would otherwise materialize a new directory straight inside the live
// checkout — ownsWorktree's EXISTING-destination check (the sibling regression test above) cannot
// catch this, since os.Lstat on a nonexistent path has nothing to compare against.
func TestResolveWorktreeRejectsLiveCheckoutRootForNewDestination(t *testing.T) {
	t.Parallel()
	main := gitRepo(t)
	repo := &Repo{Main: main, Root: main} // the symlinked-root scenario, collapsed: Root(main) == main

	dest, err := ResolveWorktree(repo, "brand-new-name")
	if err == nil {
		t.Fatalf("ResolveWorktree: want an error for a nonexistent destination under the live checkout root, got dest=%q", dest)
	}
	if _, statErr := os.Stat(filepath.Join(main, "brand-new-name")); statErr == nil {
		t.Fatal("a directory was created inside the live checkout despite the refusal")
	}
}

// TestCreateRefusesWhenWorktreeRootIsLiveCheckout is the Create-level sibling: `worktree create` must
// refuse outright rather than running `git worktree add` straight into the live checkout, even for a
// name that has never existed before.
func TestCreateRefusesWhenWorktreeRootIsLiveCheckout(t *testing.T) {
	t.Parallel()
	main := gitRepo(t)
	repo := &Repo{Main: main, Root: main}

	if dest, err := Create(repo, "brand-new-name", CreateOptions{}); err == nil {
		t.Fatalf("Create: want an error for a worktree root that is the live checkout, got dest=%q", dest)
	}
	if _, statErr := os.Stat(filepath.Join(main, "brand-new-name")); statErr == nil {
		t.Fatal("Create: a directory was created inside the live checkout despite the refusal")
	}
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
