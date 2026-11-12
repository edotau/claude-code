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
	"github.com/edotau/claude-code/internal/vault"
)

func TestDirUnreadableOriginNeverFallsBackToHome(t *testing.T) {
	tempConfig(t)
	resetVaultRetry(t)
	repo := gitRepo(t, t.TempDir(), "project")
	if _, err := EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}
	write(t, vault.Path(), "not a vault")
	if entries, err := RepoDirs(); err == nil {
		t.Fatalf("RepoDirs = %+v, nil; want an unreadable-vault error without a catalog fallback", entries)
	}

	want := filepath.Join(StateDir(), "unresolved-store")
	if got := Dir(repo); got != want {
		t.Fatalf("Dir(repo) = %q; want inert path %q, never home bank %q", got, want, HomeBankDir())
	}
}

// The harness store is keyed by the config checkout's origin repo name, else by the config dir's basename.
func TestStorePaths(t *testing.T) {
	bare := gitRepo(t, canonTemp(t, t.TempDir()), "cfg-checkout")
	t.Setenv("CLAUDE_CONFIG_DIR", bare)
	if got, want := harnessStoreDir(), filepath.Join(bare, "memory", "cfg-checkout"); got != want {
		t.Errorf("no origin: harnessStoreDir = %q, want %q", got, want)
	}
	cloned := gitRepo(t, canonTemp(t, t.TempDir()), "cfg-clone")
	hosttest.In(t, cloned, "remote", "add", "origin", "git@github.com:someone/claude-code.git")
	t.Setenv("CLAUDE_CONFIG_DIR", cloned)
	if got, want := harnessStoreDir(), filepath.Join(cloned, "memory", "claude-code"); got != want {
		t.Errorf("with origin: harnessStoreDir = %q, want %q (keyed by the remote, not the dir)", got, want)
	}
}

func TestInsideStore(t *testing.T) {
	cfg := tempConfig(t)
	cases := map[string]struct {
		rel string
		ok  bool
	}{
		filepath.Join(StoreDir(), "repo", "MEMORY.md"): {"repo/MEMORY.md", true},
		filepath.Join(StoreDir(), ".git", "config"):    {"", false},
		filepath.Join(StoreDir(), ".gitx"):             {".gitx", true},
		filepath.Join(cfg, "memory-other", "x.md"):     {"", false},
		filepath.Join(cfg, "docs", "x.md"):             {"", false},
		StoreDir():                                     {".", true},
	}
	for p, want := range cases {
		if rel, ok := insideStore(p); rel != want.rel || ok != want.ok {
			t.Errorf("insideStore(%q) = %q, %v; want %q, %v", p, rel, ok, want.rel, want.ok)
		}
	}
}

// A symlink escaping the store is outside it even though its lexical path is inside.
func TestInsideStoreRejectsEscapingSymlink(t *testing.T) {
	tempConfig(t)
	if err := os.MkdirAll(StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	os.WriteFile(outside, []byte("x"), 0o600)
	if err := os.Symlink(outside, filepath.Join(StoreDir(), "escape.md")); err != nil {
		t.Fatal(err)
	}
	if _, ok := insideStore(filepath.Join(StoreDir(), "escape.md")); ok {
		t.Error("insideStore accepted a symlink escaping the store")
	}
}

func TestMemoryRootHonorsHarnessWorktreePointer(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	worktree := t.TempDir()
	pointer := "gitdir: " + filepath.Join(cfg, ".git", "worktrees", "x") + "\n"
	write(t, filepath.Join(worktree, ".git"), pointer)
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(other)
	t.Setenv("CLAUDE_PROJECT_DIR", worktree)
	if got := root(""); got != worktree {
		t.Fatalf("root(\"\") = %q, want harness worktree %q (not cwd %q)", got, worktree, other)
	}
}

// The inverse: CLAUDE_CONFIG_DIR is itself a linked worktree, so its main checkout is not the config dir.
func TestMemoryRootHonorsConfigDirWorktree(t *testing.T) {
	main := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	pointer := "gitdir: " + filepath.Join(main, ".git", "worktrees", "x") + "\n"
	write(t, filepath.Join(cfg, ".git"), pointer)
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Chdir(other)
	t.Setenv("CLAUDE_PROJECT_DIR", cfg)
	if got := root(""); got != cfg {
		t.Fatalf("root(\"\") = %q, want config worktree %q (not cwd %q)", got, cfg, other)
	}
}

// A CLAUDE_PROJECT_DIR that is not a repo (a launcher stamping $HOME) must not win over the cwd's toplevel.
func TestMemoryRootIgnoresNonRepoProjectDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	t.Chdir(filepath.Join(repo, "sub"))
	want, _ := filepath.EvalSymlinks(repo)

	t.Setenv("CLAUDE_PROJECT_DIR", filepath.Dir(repo)) // the non-repo parent
	if got, _ := filepath.EvalSymlinks(root("")); got != want {
		t.Errorf("non-repo CLAUDE_PROJECT_DIR won: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", repo) // a real repo root is honored
	if got, _ := filepath.EvalSymlinks(root("")); got != want {
		t.Errorf("repo CLAUDE_PROJECT_DIR not honored: got %s want %s", got, want)
	}
	t.Setenv("CLAUDE_PROJECT_DIR", cfg) // the config dir itself is the global bank's root
	if got := root(""); got != cfg {
		t.Errorf("config dir not honored: got %s", got)
	}
}

func denyBankAccess(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission denial is ineffective as root")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
}

// canonTemp resolves symlinks so fixture paths match what the package resolves at runtime.
func canonTemp(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// isolatedConfig isolates CLAUDE_CONFIG_DIR, HOME and XDG_CONFIG_HOME under fresh temp dirs.
func isolatedConfig(t *testing.T) (cfg, home string) {
	t.Helper()
	cfg = canonTemp(t, t.TempDir())
	home = canonTemp(t, t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return cfg, home
}

// initStore creates the unified store's git repo; callers must already hold an isolated CLAUDE_CONFIG_DIR.
func initStore(t *testing.T) {
	t.Helper()
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
}

// gitRepo makes a real disposable repository with one commit at <parent>/<name>.
func gitRepo(t *testing.T, parent, name string) string {
	t.Helper()
	return hosttest.RepoAt(t, filepath.Join(parent, name))
}

func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	tb := &hosttest.MainTB{}
	StubSpawn(tb, true) // no memory test may launch a real detached worker
	code := m.Run()
	for _, f := range tb.Cleanups {
		f()
	}
	cleanup()
	hosttest.Cleanup()
	os.Exit(code)
}

func vaultGet(t *testing.T, key string) (string, bool) {
	t.Helper()
	var raw string
	var ok bool
	if err := vault.View(func(tx *vault.Tx) error { raw, ok = tx.Get(vault.MemoryOrigins, key); return nil }); err != nil {
		t.Fatal(err)
	}
	return raw, ok
}

// isolatedConfig1 is isolatedConfig for tests that never need the home path.
func isolatedConfig1(t *testing.T) string {
	cfg, _ := isolatedConfig(t)
	return cfg
}

// shortBankWait shrinks BankLockWait so a contended-lock test fails fast instead of waiting the 5s default.
func shortBankWait(t *testing.T) {
	t.Helper()
	old := BankLockWait
	BankLockWait = 100 * time.Millisecond
	t.Cleanup(func() { BankLockWait = old })
}

func tempConfig(t *testing.T) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	return cfg
}

func write(t testing.TB, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// repoRoot makes a git work-tree root (a .git dir is enough for the stat-first check).
func repoRoot(t *testing.T, name string) string {
	t.Helper()
	root := gitRepo(t, canonTemp(t, t.TempDir()), name)
	if _, err := EnsureRepoDir(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// clearChildEnv unsets the child-env vars so a test sees the parent as a non-child process.
func clearChildEnv(t *testing.T) {
	t.Setenv(UpdateChildEnv, "")
	t.Setenv(dreamChildEnv, "")
}

// TestRootProbesProjectDirOnce: a CLAUDE_PROJECT_DIR below the toplevel that is also cwd costs one toplevel probe.
func TestRootProbesProjectDirOnce(t *testing.T) {
	tempConfig(t)
	sub := filepath.Join(gitRepo(t, t.TempDir(), "proj"), "pkg")
	mustMkdir(t, sub)
	t.Chdir(sub)
	t.Setenv("CLAUDE_PROJECT_DIR", sub)
	want := GitToplevel(sub)
	probes := countGitProbes(t, "rev-parse --show-toplevel")
	if got := root(""); got != want || want == "" {
		t.Fatalf("root = %q, want the toplevel %q", got, want)
	}
	if *probes != 1 {
		t.Errorf("root ran the toplevel probe %d times, want 1", *probes)
	}
}

// countGitProbes wraps gitOut to count calls whose args contain probe.
func countGitProbes(t *testing.T, probe string) *int {
	t.Helper()
	n, orig := new(int), gitOut
	gitOut = func(dir string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), probe) {
			*n++
		}
		return orig(dir, args...)
	}
	t.Cleanup(func() { gitOut = orig })
	return n
}

// assertLinkedTo fails unless link is a symlink resolving to target.
func assertLinkedTo(t *testing.T, link, target string) {
	t.Helper()
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink: %v", link, err)
	}
	cur, err := os.Readlink(link)
	if err != nil || !paths.SamePath(cur, target) {
		t.Fatalf("%s -> %q, want %s (readlink err %v)", link, cur, target, err)
	}
}

// mustMkdir creates dir or fails the test.
func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// mustRemove removes path or fails the test.
func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// wantAction fails unless r.Action is want.
func wantAction(t *testing.T, label string, r linkResult, want string) {
	t.Helper()
	if r.Action != want {
		t.Fatalf("%s: %+v", label, r)
	}
}

// TestEnsureLinkStates drives every branch of ensureLink: absent, a correct link, a wrong link, an empty
// real dir, and a non-empty real dir.
func TestEnsureLinkStates(t *testing.T) {
	t.Parallel()
	cfg := canonTemp(t, t.TempDir())
	target := filepath.Join(cfg, "memory", "target")
	mustMkdir(t, target)
	link := filepath.Join(cfg, "projects", "proj", "memory")

	// absent -> linked
	wantAction(t, "absent", ensureLink(link, target), "linked")
	assertLinkedTo(t, link, target)

	// correct link -> ok
	wantAction(t, "existing correct link", ensureLink(link, target), "ok")

	// wrong link -> relinked
	other := filepath.Join(cfg, "memory", "other")
	mustMkdir(t, other)
	r := ensureLink(link, other)
	wantAction(t, "wrong link", r, "relinked")
	if !strings.Contains(r.Detail, target) {
		t.Fatalf("wrong link: %+v", r)
	}
	assertLinkedTo(t, link, other)

	// reset to a real, empty dir -> replaced by a link
	mustRemove(t, link)
	mustMkdir(t, link)
	wantAction(t, "empty real dir", ensureLink(link, target), "linked")
	assertLinkedTo(t, link, target)

	// reset to a real, non-empty dir -> blocked, content untouched
	mustRemove(t, link)
	content := filepath.Join(link, "note.md")
	write(t, content, "real content\n")
	wantAction(t, "non-empty dir", ensureLink(link, target), "blocked")
	if got := mustRead(t, content); got != "real content\n" {
		t.Errorf("content touched by a blocked link: %q", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Error("a blocked non-empty dir must stay a real directory")
	}
}

// TestLinkNativeForNoopBeforeCutover: linkNativeFor reports ok=false before the migration lands.
func TestLinkNativeForNoopBeforeCutover(t *testing.T) {
	isolatedConfig(t)
	if _, ok := linkNativeFor(t.TempDir()); ok {
		t.Error("linkNativeFor before cutover must report ok=false")
	}
}

func TestEnsureLinkPreservesCorrectRelativeTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "store")
	mustMkdir(t, target)
	link := filepath.Join(dir, "memory")
	if err := os.Symlink("store", link); err != nil {
		t.Fatal(err)
	}
	wantAction(t, "correct relative link", ensureLink(link, target), "ok")
	if got, err := os.Readlink(link); err != nil || got != "store" {
		t.Fatalf("correct relative link was rewritten: %q, %v", got, err)
	}
}

// TestLinkNativeForProbesGitOnce: linkNativeFor hands its main-checkout probe to RepoDir instead of running it twice.
func TestLinkNativeForProbesGitOnce(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	identities.Delete(filepath.Clean(root)) // the store's creation memoized it; linkNativeFor must resolve afresh
	probes := countGitProbes(t, "--git-common-dir")
	res, ok := linkNativeFor(root)
	if !ok || res.Target != mustRepoDir(t, root) || res.Action != "linked" {
		t.Fatalf("linkNativeFor = %+v, %v, want a link to the repo's store", res, ok)
	}
	if *probes != 1 {
		t.Errorf("linkNativeFor ran the main-checkout probe %d times, want 1", *probes)
	}
}
