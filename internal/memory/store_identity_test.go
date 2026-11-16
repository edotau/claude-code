package memory

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
	if want.HomeBank || want.Harness || want.Base != "proj" || want.Origin != paths.CanonDeep(repo) {
		t.Fatalf("Resolve(repo) = %+v", want)
	}
	for _, root := range []string{sub, wt, filepath.Join(wt, "a")} {
		os.MkdirAll(root, 0o755)
		if got := Resolve(root); got != want {
			t.Errorf("Resolve(%s) = %+v want %+v", root, got, want)
		}
	}
	if got := Resolve(t.TempDir()); !got.HomeBank || got.Origin != "" {
		t.Errorf("non-repo must be global, got %+v", got)
	}
}

type mainCheckoutCase struct{ root, lexical, git string }

// mainCheckoutCases builds the repo, worktree, symlink, separate-gitdir and bare layouts MainCheckout must tell apart.
func mainCheckoutCases(t *testing.T) []mainCheckoutCase {
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
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, repo)
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
		if !id.Harness || id.HomeBank || id.Origin != paths.CanonDeep(cfg) {
			t.Errorf("Resolve(%s) = %+v", root, id)
		}
		if b := BankFor(root); b.Key != harnessStoreKey() {
			t.Errorf("BankFor(%s) = %+v, want the harness bank", root, b)
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
	if b := BankFor(worktree); b.Key != harnessStoreKey() {
		t.Errorf("BankFor(worktree) = %+v, want the harness bank", b)
	}
}

// The config dir is itself a linked worktree of another checkout.
func TestResolveConfigDirWorktree(t *testing.T) {
	main := t.TempDir()
	cfg := tempConfig(t)
	write(t, filepath.Join(cfg, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "x")+"\n")
	if got := Resolve(cfg); !got.Harness {
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
	if got := Resolve(a); got.Base != "proj" || got.Origin != paths.CanonDeep(a) {
		t.Errorf("Resolve under hostile GIT_DIR/GIT_WORK_TREE = %+v, want proj", got)
	}
}

// countRemoteLookups puts a git shim on PATH that logs each `remote get-url` call, and returns the count func.
func countRemoteLookups(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	shim := "#!/bin/sh\ncase \"$*\" in *\"remote get-url\"*) echo x >> '" + log + "';; esac\nexec '" + real + "' \"$@\"\n"
	idFatal(t, os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "x")
	}
}

// resetRemoteCache drops the memoized remotes.
func resetRemoteCache() { remoteCache.Clear() }

// The second lookup for one checkout (hit or miss) forks no git; the reset hook clears it.
func TestOriginRemoteURLIsMemoized(t *testing.T) {
	tempConfig(t)
	withRemote := gitRepo(t, t.TempDir(), "has-remote")
	hosttest.In(t, withRemote, "remote", "add", "origin", "https://github.com/tester/memo.git")
	noRemote := gitRepo(t, t.TempDir(), "no-remote")
	resetRemoteCache()
	t.Cleanup(resetRemoteCache)
	calls := countRemoteLookups(t)
	for _, c := range []struct{ dir, want string }{
		{withRemote, "https://github.com/tester/memo.git"}, {noRemote, ""},
	} {
		before := calls()
		if got := originRemoteURL(c.dir); got != c.want {
			t.Fatalf("originRemoteURL(%s) = %q, want %q", c.dir, got, c.want)
		}
		if calls()-before != 1 {
			t.Fatalf("first lookup of %s forked git %d times, want 1", c.dir, calls()-before)
		}
		before = calls()
		if got := originRemoteURL(c.dir); got != c.want {
			t.Fatalf("cached originRemoteURL = %q, want %q", got, c.want)
		}
		if calls() != before {
			t.Fatalf("second lookup of %s forked git again", c.dir)
		}
	}
	hosttest.In(t, noRemote, "remote", "add", "origin", "https://github.com/tester/later.git")
	if got := originRemoteURL(noRemote); got != "" {
		t.Fatalf("cache should hide the new remote, got %q", got)
	}
	resetRemoteCache()
	if got := originRemoteURL(noRemote); got != "https://github.com/tester/later.git" {
		t.Fatalf("after reset, originRemoteURL = %q, want the new remote", got)
	}
}

func TestRepoKeyFromRemote(t *testing.T) {
	t.Parallel()
	cases := []struct{ url, want string }{
		{"ssh://git@ssh.github.com:443/tester/claude-code.git", "claude-code"},
		{"git@github.com:tester/claude-dbx.git", "claude-dbx"},
		{"https://github.com/tester/claude-other.git", "claude-other"},
		{"https://github.com/tester/tester.github.io.git", "tester.github.io"},
		{"https://github.com/tester/claude-code", "claude-code"},      // no .git suffix
		{"https://github.com/tester/claude-code.git/", "claude-code"}, // trailing slash
		{"", ""},
	}
	for _, c := range cases {
		if got := repoKeyFromRemote(c.url); got != c.want {
			t.Errorf("repoKeyFromRemote(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// A git repo in a scratch folder never becomes its own store: it takes the launching workspace, else home.
func TestThrowawayRepoUsesLaunchingWorkspace(t *testing.T) {
	tempConfig(t)
	orig := throwawayInTests
	throwawayInTests = true
	t.Cleanup(func() { throwawayInTests = orig })
	scratch := gitRepo(t, t.TempDir(), "p4-opencode-fix1")
	t.Setenv(OriginEnv, "")
	if id := Resolve(scratch); !id.HomeBank {
		t.Errorf("no %s: Resolve = %+v, want the home tier", OriginEnv, id)
	}
	ws := paths.CanonDeep(t.TempDir())
	t.Setenv(OriginEnv, ws)
	if id := Resolve(scratch); id.HomeBank || id.Origin != ws {
		t.Errorf("%s=%s: Resolve = %+v, want that workspace", OriginEnv, ws, id)
	}
	if got := LaunchOrigin(scratch); got != ws {
		t.Errorf("LaunchOrigin = %q, want the inherited %q", got, ws)
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

// The bank key comes from the origin remote's basename, not the dir name; a remote naming no repo uses the dir.
func TestBankKeyFromOriginRemoteOrDir(t *testing.T) {
	for _, c := range []struct{ name, dirName, remote, key string }{
		{"remote beats dir name", "mismatched-dirname", "https://github.com/tester/claude-dbx.git", "claude-dbx"},
		{"bare slash remote", "no-remote-repo", "/", "no-remote-repo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolatedConfig(t)
			repo := gitRepo(t, t.TempDir(), c.dirName)
			hosttest.In(t, repo, "remote", "add", "origin", c.remote)
			if got := BankFor(repo).Key; got != c.key {
				t.Fatalf("BankFor key = %q, want %q", got, c.key)
			}
		})
	}
}

// A config that [include]s another file is never cached on disk: the included file is not in the stamp.
func TestOriginRemoteURLSeesIncludedConfigChange(t *testing.T) {
	tempConfig(t)
	for _, e := range os.Environ() { // the test host's GIT_CONFIG_* env would disable the disk cache under test
		if k, _, _ := strings.Cut(e, "="); strings.HasPrefix(k, "GIT_CONFIG") {
			t.Setenv(k, "")
			idFatal(t, os.Unsetenv(k))
		}
	}
	repo := gitRepo(t, t.TempDir(), "inc")
	included := filepath.Join(repo, "origin.conf")
	write := func(name string) {
		idFatal(t, os.WriteFile(included, []byte("[remote \"origin\"]\n url = https://example.invalid/"+name+".git\n"), 0o600))
	}
	cfg, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0o600)
	idFatal(t, err)
	_, err = cfg.WriteString("\n[include]\n\tpath = " + included + "\n")
	idFatal(t, err)
	idFatal(t, cfg.Close())
	resetRemoteCache()
	t.Cleanup(resetRemoteCache)
	write("one")
	if got := originRemoteURL(repo); got != "https://example.invalid/one.git" {
		t.Fatalf("first originRemoteURL = %q, want one", got)
	}
	write("two")
	resetRemoteCache() // a fresh hook process: only the on-disk cache survives
	if got := originRemoteURL(repo); got != "https://example.invalid/two.git" {
		t.Fatalf("after the included file changed, originRemoteURL = %q, want two", got)
	}
}
