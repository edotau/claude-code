package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

func idFatal(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func idMkdir(t *testing.T, dir string) {
	t.Helper()
	idFatal(t, os.MkdirAll(dir, 0o755))
}

func TestResolveSharesWorktreeAndSubdir(t *testing.T) {
	tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	sub := filepath.Join(repo, "a", "b")
	idMkdir(t, sub)
	wt := filepath.Join(t.TempDir(), "wt")
	hosttest.In(t, repo, "worktree", "add", "-q", wt)
	want := Resolve(repo)
	if want.Origin != paths.CanonDeep(repo) {
		t.Fatalf("Resolve(repo) = %+v", want)
	}
	for _, root := range []string{sub, wt, filepath.Join(wt, "a")} {
		os.MkdirAll(root, 0o755)
		if got := Resolve(root); got != want {
			t.Errorf("Resolve(%s) = %+v want %+v", root, got, want)
		}
	}
	if got := Resolve(t.TempDir()); got.Origin != "" {
		t.Errorf("non-repo must be global, got %+v", got)
	}
}

type mainCheckoutCase struct{ root, lexical, git string }

// mainCheckoutCases builds the repo, worktree, symlink, separate-gitdir and bare layouts MainCheckout must tell apart.
func mainCheckoutCases(t *testing.T) []mainCheckoutCase {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	t.Helper()
	base := t.TempDir()
	repo := gitRepo(t, base, "repo")
	wt, separate, bare := filepath.Join(base, "wt"), filepath.Join(base, "separate"), filepath.Join(base, "bare")
	hosttest.In(t, repo, "worktree", "add", "-q", wt)
	write(t, filepath.Join(wt, ".git"), "gitdir: ../repo/.git/worktrees/wt\n")
	hosttest.In(t, base, "init", "-q", "--separate-git-dir="+filepath.Join(base, "gitdata"), separate)
	hosttest.In(t, base, "init", "-q", "--bare", bare)
	for _, dir := range []string{filepath.Join(repo, "sub"), filepath.Join(wt, "sub"), filepath.Join(base, "plain")} {
		idMkdir(t, dir)
	}
	link := filepath.Join(base, "link")
	idFatal(t, os.Symlink(repo, link))
	idFatal(t, os.Symlink(wt, wt+"-link"))
	t.Chdir(base) // a cwd-relative path stays short wherever TMPDIR lives (ramdisk, other volume)
	rel, err := filepath.Rel(base, repo)
	if err != nil {
		t.Fatal(err)
	}
	return []mainCheckoutCase{
		{repo, repo, repo}, {filepath.Join(repo, "sub"), filepath.Join(repo, "sub"), repo},
		{wt, repo, repo}, {filepath.Join(wt, "sub"), filepath.Join(wt, "sub"), repo}, {wt + "-link", repo, repo},
		{link, link, repo}, {rel, rel, repo}, {filepath.Join(filepath.Dir(rel), "wt"), rel, repo},
		{separate, separate, separate}, {bare, bare, ""},
		{filepath.Join(base, "plain"), filepath.Join(base, "plain"), ""},
		{filepath.Join(base, "missing"), filepath.Join(base, "missing"), ""},
	}
}

// canonical resolves p through symlinks unless it is empty.
func canonical(p string) string {
	if p == "" {
		return ""
	}
	return paths.CanonDeep(p) // git reports the real path: macOS TempDir /var is /private/var
}

func TestMainCheckoutModes(t *testing.T) {
	tempConfig(t)
	for _, tc := range mainCheckoutCases(t) {
		if got := paths.MainCheckout(tc.root); got != tc.lexical {
			t.Errorf("MainCheckout(%q) = %q, want lexical %q", tc.root, got, tc.lexical)
		}
		got, want := canonical(paths.MainCheckout(tc.root, gitOut)), canonical(tc.git)
		if got != want {
			t.Errorf("MainCheckout(%q, GitOut) = %q, want canonical %q", tc.root, got, want)
		}
	}
}

func TestResolveHarness(t *testing.T) {
	cfg := gitRepo(t, t.TempDir(), ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sub := filepath.Join(cfg, "internal", "memory")
	idMkdir(t, sub)
	for _, root := range []string{cfg, sub} {
		id := Resolve(root)
		if id.Origin != paths.CanonDeep(cfg) {
			t.Errorf("Resolve(%s) = %+v", root, id)
		}

	}
}

func TestResolveHarnessWorktreePointer(t *testing.T) {
	cfg := tempConfig(t)
	worktree := t.TempDir()
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(cfg, ".git", "worktrees", "x")+"\n")
	want := Resolve(cfg)
	if got := Resolve(worktree); got != want {
		t.Errorf("Resolve(worktree) = %+v, want main checkout identity %+v", got, want)
	}
}

// The config dir is itself a linked worktree of another checkout.
func TestResolveConfigDirWorktree(t *testing.T) {
	main := t.TempDir()
	cfg := tempConfig(t)
	write(t, filepath.Join(cfg, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "x")+"\n")
	if got := Resolve(cfg); got.Origin != paths.CanonDeep(cfg) {
		t.Errorf("Resolve(config worktree) = %+v, want harness identity", got)
	}
}

// A hostile GIT_DIR/GIT_WORK_TREE pointing at a decoy cannot redirect resolution: runGit strips inherited GIT_*.
func TestResolveIgnoresInheritedGitEnv(t *testing.T) {
	tempConfig(t)
	a := gitRepo(t, t.TempDir(), "proj")
	decoy := gitRepo(t, t.TempDir(), "decoy")
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_WORK_TREE", decoy)
	if got := Resolve(a); got.Origin != paths.CanonDeep(a) {
		t.Errorf("Resolve under hostile GIT_DIR/GIT_WORK_TREE = %+v, want proj", got)
	}
}

// Pins the physical lock file and guards against a ".lock.lock" variant.
func TestBankLockPathLocation(t *testing.T) {
	cfg := tempConfig(t)
	p := BankLockPath()
	want := filepath.Join(cfg, "state", "harness", "memory", "bank.lock")
	if p != want {
		t.Fatalf("BankLockPath() = %q, want %q", p, want)
	}
	if strings.HasSuffix(p, ".lock.lock") {
		t.Fatalf("BankLockPath() produced a .lock.lock variant: %q", p)
	}
	if err := WithBankLock(10*time.Millisecond, func() error { return nil }); err != nil {
		t.Fatalf("WithBankLock: %v", err)
	}
	if _, err := os.Stat(p + ".lock"); err == nil {
		t.Fatalf("a %s.lock file must never appear", p)
	}
}

// A held OS file lock yields ErrBankBusy without running fn; once released fn runs.
func TestWithBankLockContention(t *testing.T) {
	tempConfig(t)
	path := BankLockPath()
	idFatal(t, os.MkdirAll(filepath.Dir(path), 0o700))
	ran := false
	held, err := paths.TryFileLock(path, 0, func() error {
		return WithBankLock(10*time.Millisecond, func() error { ran = true; return nil })
	})
	if !held {
		t.Fatalf("could not take the bank lock for the holder: %v", err)
	}
	if err != ErrBankBusy {
		t.Fatalf("WithBankLock while held = %v, want ErrBankBusy", err)
	}
	if ran {
		t.Fatal("fn must not run while the bank lock is held elsewhere")
	}
	if err := WithBankLock(10*time.Millisecond, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("WithBankLock after release: %v", err)
	}
	if !ran {
		t.Fatal("fn must run once the bank lock is released")
	}
}

// A routed name must appear as a whole word; a substring of it, or a key under 4 chars, never matches.
func TestRouteMatchWholeWordAndShortKeysIgnored(t *testing.T) {
	for _, c := range []struct {
		query, key string
		want       bool
	}{
		{"check Tester.GitHub.io, then deploy", "tester.github.io", true},
		{"check .project", ".project", true},
		{"my-tester.github.io", "tester.github.io", false},
		{"tester.github.ion", "tester.github.io", false},
		{"abc is here", "abc", false},
	} {
		if got := routeMatch(strings.ToLower(c.query), c.key); got != c.want {
			t.Errorf("routeMatch(%q, %q) = %v, want %v", c.query, c.key, got, c.want)
		}
	}
}
