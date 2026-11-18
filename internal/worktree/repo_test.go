package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

// gitRepo makes a fresh git repo with one commit, env wired so every git invocation has an identity.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := paths.Canon(hosttest.Repo(t))
	return dir
}

// testRepo builds an isolated Repo with its own worktree root without mutating HOME.
func testRepo(t *testing.T) *Repo {
	t.Helper()
	main := gitRepo(t)
	root := paths.Canon(t.TempDir())
	return &Repo{Main: main, Root: filepath.Join(root, "worktrees", filepath.Base(main))}
}

func testDetachedRepo(t *testing.T) *Repo {
	t.Helper()
	main := detachedHead(t)
	root := paths.Canon(t.TempDir())
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
	// worktreeRoot canonicalizes HOME
	canon, _ := filepath.EvalSymlinks(home)
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
		if _, err := resolveWorktreeBounded(repo, name); err == nil {
			t.Errorf("name %q: want error, got nil", name)
		}
	}
}

// A refused name's message states the rule and, when one exists, a usable name to retry with.
func TestCheckWorktreeNameMessageNamesRuleAndFix(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	for name, try := range map[string]string{
		"feat/x": `try "feat-x"`, `a\b`: `try "a-b"`, "/lead/": `try "lead"`, "": "", ".": "", "..": "", "/": "",
	} {
		_, err := checkWorktreeName(repo, name)
		if err == nil || !strings.Contains(err.Error(), "one path segment") {
			t.Errorf("name %q: err %v, want the naming rule", name, err)
			continue
		}
		if got := strings.Contains(err.Error(), "try "); got != (try != "") || !strings.Contains(err.Error(), try) {
			t.Errorf("name %q: err %v, want suggestion %q", name, err, try)
		}
	}
}

func TestResolveWorktreeAcceptsUnusualButSafeNames(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	for _, name := range []string{"has space", "dots.and.more", "unicode-éé", "UPPER-case_123", "feat-x", "..-escape"} {
		dest, err := resolveWorktreeBounded(repo, name)
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
	if _, err := resolveWorktreeBounded(repo, "taken"); err == nil {
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
	if _, err := resolveWorktreeBounded(repo, "other"); err == nil {
		t.Fatal("want collision error for a foreign repo occupying the name")
	}
}

func TestResolveWorktreeSymlinkAtDestination(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
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
	if _, err := resolveWorktreeBounded(repo, "link"); err == nil {
		t.Fatal("want error for a symlink at the destination")
	}
}

func TestCleanAtRefusesSymlinkedDest(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Parallel()
	repo := testRepo(t)
	if err := os.MkdirAll(repo.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(repo.Root, "swapped")
	if err := os.Symlink(target, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanAtWith(repo, "swapped", dest, false, nil, LedgerClose{Via: "clean"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("cleanAt on a symlinked dest: err = %v, want a symlink refusal", err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Fatalf("symlink target was touched: %v", err)
	}
}

func TestResolveWorktreeResumesExisting(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	dest, err := Create(repo, "resume", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := resolveWorktreeBounded(repo, "resume")
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
	// worktreeRoot canonicalizes HOME
	home, _ = filepath.EvalSymlinks(home)
	want := filepath.Join(home, "worktrees", "myrepo")
	if got != want {
		t.Fatalf("Root = %q, want %q", got, want)
	}
}

func TestWorktreeRootUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// worktreeRoot canonicalizes HOME
	home, _ = filepath.EvalSymlinks(home)
	if got, want := worktreeRoot(), filepath.Join(home, "worktrees"); got != want {
		t.Fatalf("worktreeRoot = %q, want %q", got, want)
	}
}

// detachedHead is a minimal repo whose main checkout is in detached-HEAD state.
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

// Regression: under a root symlinked onto the live checkout, an existing subdirectory is not a worktree.
func TestResolveWorktreeRejectsLiveCheckoutSubdirUnderSymlinkedRoot(t *testing.T) {
	t.Parallel()
	main := gitRepo(t)
	// repo.Root equal to main is what the symlinked-root scenario collapses to, so no real symlink is needed.
	repo := &Repo{Main: main, Root: main}

	internal := filepath.Join(main, "internal")
	if err := os.MkdirAll(internal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(internal, "source.go"), []byte("package internal\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveWorktreeBounded(repo, "internal"); err == nil {
		t.Fatal("ResolveWorktree: want an error — the live checkout's own subdirectory must never be treated as a worktree")
	}
}

// A new name under a root symlinked into the live checkout must be refused before Create makes a directory.
func TestResolveWorktreeRejectsLiveCheckoutRootForNewDestination(t *testing.T) {
	t.Parallel()
	main := gitRepo(t)
	repo := &Repo{Main: main, Root: main}

	dest, err := resolveWorktreeBounded(repo, "brand-new-name")
	if err == nil {
		t.Fatalf("ResolveWorktree: want an error for a nonexistent destination under the live checkout root, got dest=%q", dest)
	}
	if _, statErr := os.Stat(filepath.Join(main, "brand-new-name")); statErr == nil {
		t.Fatal("a directory was created inside the live checkout despite the refusal")
	}
}

// `worktree create` must refuse outright when the worktree root is the live checkout, even for a never-used name.
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

func TestGitTimeoutCtxHasDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > probeTimeout {
		t.Fatalf("deadline = %v, %v; want one within %v", dl, ok, probeTimeout)
	}
}

func TestCleanAtWarnsWhenBranchNotDetected(t *testing.T) {
	t.Parallel()
	repo := testRepo(t)
	// not a git worktree, so `git branch --show-current` fails
	dest := t.TempDir()
	var log strings.Builder
	_, _ = cleanAtWith(repo, "nobranch", dest, true, &log, LedgerClose{Via: "clean"})
	if !strings.Contains(log.String(), "branch not detected") {
		t.Fatalf("log = %q, want a branch-not-detected warning", log.String())
	}
}

// TestMain pins state and config paths to a temp dir so no test touches the real ~/.claude, and trims git startup.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	StubProcessScanForTest()
	code := m.Run()
	cleanup()
	hosttest.Cleanup()
	os.Exit(code)
}
