package memory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

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
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
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
	repo := StoreRepo()
	if err := repo.plainDir(); err != nil {
		t.Fatal(err)
	}
	if !repo.Exists() { // a copy of the template whose root check passed; Ensure below still refuses remotes
		gitDir := filepath.Join(repo.Dir, ".git")
		if err := os.CopyFS(gitDir, os.DirFS(storeTemplate(t))); err != nil {
			t.Fatal(err)
		}
		markCopied(gitDir)
	}
	if err := repo.Ensure(); err != nil {
		t.Fatal(err)
	}
}

// storeTmpl is the dir whose .git one real Ensure built in TestMain; TestMain removes it.
var storeTmpl struct {
	dir string
	err error
}

// realInitDirs holds store dirs whose test needs the real `git init` path of Ensure.
var realInitDirs sync.Map

// copyInit is gitInit for tests: a copy of the template .git, marked root-verified like initStore does.
func copyInit(real func(storeRepo) error) func(storeRepo) error {
	return func(r storeRepo) error {
		if _, ok := realInitDirs.Load(r.Dir); ok || r.Dir == storeTmpl.dir {
			return real(r)
		}
		gitDir := filepath.Join(r.Dir, ".git")
		if err := os.CopyFS(gitDir, os.DirFS(filepath.Join(storeTmpl.dir, ".git"))); err != nil {
			return err
		}
		markCopied(gitDir)
		return nil
	}
}

// markCopied records a template copy as root-verified and remote-free; a later config edit changes the stamp.
func markCopied(gitDir string) {
	key := paths.CanonDeep(gitDir)
	verifiedRoots.Store(key, struct{}{})
	remoteFree.Store(key, configStamp(gitDir))
}

func storeTemplate(t *testing.T) string {
	t.Helper()
	if storeTmpl.err != nil {
		t.Fatal(storeTmpl.err)
	}
	return filepath.Join(storeTmpl.dir, ".git")
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
	if storeTmpl.dir, storeTmpl.err = os.MkdirTemp("", "memstore-"); storeTmpl.err == nil {
		storeTmpl.err = storeRepo{Dir: storeTmpl.dir}.Ensure()
	}
	if storeTmpl.err == nil { // git's inert sample hooks: fewer files for every per-test copy to write and remove
		samples, _ := filepath.Glob(filepath.Join(storeTmpl.dir, ".git", "hooks", "*.sample"))
		for _, s := range samples {
			storeTmpl.err = errors.Join(storeTmpl.err, os.Remove(s))
		}
	}
	gitInit = copyInit(gitInit)
	code := m.Run()
	for _, f := range tb.Cleanups {
		f()
	}
	cleanup()
	hosttest.Cleanup()
	if storeTmpl.dir != "" {
		_ = os.RemoveAll(storeTmpl.dir)
	}
	os.Exit(code)
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
	if err := EnsureBank(""); err != nil {
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

// TestEnsureLinkStates drives ensureLink over absent, correct, wrong, empty-dir and non-empty-dir states.
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

// TestLinkNativeForNoopWithoutStore: linkNativeFor reports ok=false while the store dir does not exist.
func TestLinkNativeForNoopWithoutStore(t *testing.T) {
	isolatedConfig(t)
	if err := os.RemoveAll(paths.MemoryStoreDir()); err != nil {
		t.Fatal(err)
	}
	if _, ok := linkNativeFor(t.TempDir()); ok {
		t.Error("linkNativeFor without a store must report ok=false")
	}
}

func TestEnsureLinkPreservesCorrectRelativeTarget(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
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

// TestLinkNativeForProbesGitOnce: linkNativeFor probes git once.
func TestLinkNativeForProbesGitOnce(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	identities.Delete(filepath.Clean(root)) // the store's creation memoized it; linkNativeFor must resolve afresh
	probes := countGitProbes(t, "--git-common-dir")
	res, ok := linkNativeFor(root)
	if !ok || res.Target != paths.MemoryStoreDir() || res.Action != "linked" {
		t.Fatalf("linkNativeFor = %+v, %v, want a link to the repo's store", res, ok)
	}
	if *probes != 1 {
		t.Errorf("linkNativeFor ran the main-checkout probe %d times, want 1", *probes)
	}
}

// runRoot runs fn with a fresh git repo "proj" in an isolated flat config.
func runRoot(t *testing.T, fn func(t *testing.T, root string)) {
	t.Helper()
	isolatedConfig(t)
	fn(t, gitRepo(t, canonTemp(t, t.TempDir()), "proj"))
}

func TestInitSeedsTheBank(t *testing.T) {
	runRoot(t, func(t *testing.T, root string) {
		dir, err := Init()
		if err != nil {
			t.Fatal(err)
		}
		if dir != paths.MemorySubDir("") {
			t.Fatalf("Init = %s, want its MemorySubDir", dir)
		}
		for _, name := range bankFiles {
			if !statOK(Path(name)) {
				t.Errorf("Init left %s missing", Path(name))
			}
		}
		if dir != paths.MemoryStoreDir() || filepath.Base(Path("progress.md")) != "progress.md" {
			t.Errorf("dir %s and file %s disagree with the flat layout", dir, Path("progress.md"))
		}
	})
}

func TestNativeLinksShareTheStoreRoot(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	runRoot(t, func(t *testing.T, root string) {
		if _, err := Init(); err != nil {
			t.Fatal(err)
		}
		res, ok := linkNativeFor(root)
		wantTarget := paths.MemorySubDir("")
		if !ok || res.Target != wantTarget {
			t.Fatalf("linkNativeFor = %+v, %v, want target %s", res, ok, wantTarget)
		}
		if got := nativeLinkTargets(root)[paths.ProjectSlug(paths.ConfigDir())]; got != paths.MemoryStoreDir() {
			t.Errorf("harness slug target = %q, want the store root", got)
		}
		link := projectsMemory("-stale-slug")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(paths.MemoryStoreDir(), link); err != nil {
			t.Fatal(err)
		}
		flagged := slices.ContainsFunc(brokenNativeLinks(root), func(r linkResult) bool { return r.Link == link })
		if flagged {
			t.Error("a link to the store root was flagged broken")
		}
	})
}

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
	want := resolve(repo)
	if want != paths.CanonDeep(repo) {
		t.Fatalf("Resolve(repo) = %+v", want)
	}
	for _, root := range []string{sub, wt, filepath.Join(wt, "a")} {
		os.MkdirAll(root, 0o755)
		if got := resolve(root); got != want {
			t.Errorf("Resolve(%s) = %+v want %+v", root, got, want)
		}
	}
	if got := resolve(t.TempDir()); got != "" {
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
		id := resolve(root)
		if id != paths.CanonDeep(cfg) {
			t.Errorf("Resolve(%s) = %+v", root, id)
		}

	}
}

func TestResolveHarnessWorktreePointer(t *testing.T) {
	cfg := tempConfig(t)
	worktree := t.TempDir()
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(cfg, ".git", "worktrees", "x")+"\n")
	want := resolve(cfg)
	if got := resolve(worktree); got != want {
		t.Errorf("Resolve(worktree) = %+v, want main checkout identity %+v", got, want)
	}
}

// The config dir is itself a linked worktree of another checkout.
func TestResolveConfigDirWorktree(t *testing.T) {
	main := t.TempDir()
	cfg := tempConfig(t)
	write(t, filepath.Join(cfg, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "x")+"\n")
	if got := resolve(cfg); got != paths.CanonDeep(cfg) {
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
	if got := resolve(a); got != paths.CanonDeep(a) {
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

func TestDatedNameAndStripRepoPrefix(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"repo--2026-01-02-x.md": "2026-01-02-x.md",
		"20260102-x.md":         "2026-01-02-x.md",
		"x-2026-03-04.md":       "2026-03-04-x-2026-03-04.md",
		"plain.md":              "2026-11-25-plain.md",
	} {
		if got := DatedName(in, now); got != want {
			t.Errorf("DatedName(%q) = %q, want %q", in, got, want)
		}
	}
	if rest, key := StripRepoPrefix("a--b--c.md"); rest != "c.md" || key != "a" {
		t.Errorf("StripRepoPrefix = %q, %q", rest, key)
	}
	if rest, key := StripRepoPrefix("plain.md"); rest != "plain.md" || key != "" {
		t.Errorf("StripRepoPrefix plain = %q, %q", rest, key)
	}
}
