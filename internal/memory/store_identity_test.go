package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// fixtureGit runs git on dir minus inherited GIT_* (GIT_DIR beats -C), user/system config, hooks and signing.
func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
		"-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(proc.GitEnv(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestResolveSharesWorktreeAndSubdir(t *testing.T) {
	tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "proj")
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	fixtureGit(t, repo, "worktree", "add", "-q", wt)
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
	fixtureGit(t, repo, "worktree", "add", "-q", wt)
	write(t, filepath.Join(wt, ".git"), "gitdir: ../repo/.git/worktrees/wt\n")
	fixtureGit(t, base, "init", "-q", "--separate-git-dir="+filepath.Join(base, "gitdata"), separate)
	fixtureGit(t, base, "init", "-q", "--bare", bare)
	for _, dir := range []string{filepath.Join(repo, "sub"), filepath.Join(wt, "sub"), filepath.Join(base, "plain")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wt, wt+"-link"); err != nil {
		t.Fatal(err)
	}
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
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{cfg, sub} {
		assertHarnessRoot(t, cfg, root)
	}
	dir, err := EnsureRepoDir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	o, err := ReadOrigin(dir)
	if err != nil || o.Key != harnessStoreKey() || o.Origin != paths.CanonDeep(cfg) {
		t.Errorf("harness .origin = %+v, %v", o, err)
	}
}

// assertHarnessRoot checks root resolves to the harness identity and store dir.
func assertHarnessRoot(t *testing.T, cfg, root string) {
	t.Helper()
	id := Resolve(root)
	if !id.Harness || id.HomeBank || id.Origin != paths.CanonDeep(cfg) {
		t.Errorf("Resolve(%s) = %+v", root, id)
	}
	if dir, err := RepoDir(root); err != nil || dir != harnessStoreDir() {
		t.Errorf("RepoDir(%s) = %s, %v", root, dir, err)
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
	if got, err := RepoDir(worktree); err != nil || got != harnessStoreDir() {
		t.Errorf("RepoDir(worktree) = %q, %v, want %q", got, err, harnessStoreDir())
	}
}

// The inverse: the config dir is itself a linked worktree of another checkout.
func TestResolveConfigDirWorktree(t *testing.T) {
	main := t.TempDir()
	cfg := tempConfig(t)
	write(t, filepath.Join(cfg, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "x")+"\n")

	if got := Resolve(cfg); !got.Harness {
		t.Errorf("Resolve(config worktree) = %+v, want harness identity", got)
	}
}

func TestRepoDirReadOnly(t *testing.T) {
	cfg := tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "My Repo.v2")
	dir, err := RepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "memory", "my-repo.v2"); dir != want {
		t.Errorf("RepoDir = %s want %s", dir, want)
	}
	if _, err := os.Stat(StoreDir()); !errors.Is(err, os.ErrNotExist) {
		t.Error("RepoDir must not create the store")
	}
	if plain := t.TempDir(); mustRepoDir(t, plain) != homeStoreDir() {
		t.Error("non-repo must map to the global store dir")
	}
}

func TestEnsureRepoDirIdempotent(t *testing.T) {
	tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "My Repo.v2")
	dir := mustRepoDir(t, repo)
	got, err := EnsureRepoDir(repo)
	if err != nil || got != dir {
		t.Fatalf("EnsureRepoDir = %s, %v", got, err)
	}
	o, err := ReadOrigin(dir)
	if err != nil || o.Origin != paths.CanonDeep(repo) || o.Key != "my-repo.v2" {
		t.Fatalf("origin = %+v, %v", o, err)
	}
	if again, err := EnsureRepoDir(repo); err != nil || again != dir {
		t.Fatal(err)
	}
	if after, err := ReadOrigin(dir); err != nil || !reflect.DeepEqual(after, o) {
		t.Errorf("second EnsureRepoDir rewrote the origin record: %+v, %v", after, err)
	}
}

func TestEnsureRepoDirGlobalTierHasNoOrigin(t *testing.T) {
	tempConfig(t)
	if g, err := EnsureRepoDir(t.TempDir()); err != nil || g != homeStoreDir() {
		t.Errorf("global EnsureRepoDir = %s, %v", g, err)
	}
	if _, err := ReadOrigin(homeStoreDir()); err == nil {
		t.Error("no origin record for the global tier")
	}
}

func mustRepoDir(t *testing.T, root string) string {
	t.Helper()
	dir, err := RepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSameBasenameCollisionExtendsHash(t *testing.T) {
	cfg := tempConfig(t)
	a := gitRepo(t, t.TempDir(), "app")
	b := gitRepo(t, t.TempDir(), "app")
	da, err := EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	if da != filepath.Join(cfg, "memory", "app") || !strings.HasPrefix(filepath.Base(db), "app--") || len(filepath.Base(db)) != len("app--")+8 {
		t.Fatalf("dirs = %s, %s", da, db)
	}
	if mustRepoDir(t, a) != da || mustRepoDir(t, b) != db {
		t.Error("RepoDir must find both stores by origin")
	}
	if oa, _ := ReadOrigin(da); oa.Origin != paths.CanonDeep(a) {
		t.Errorf("first store's origin was clobbered: %+v", oa)
	}
}

// A foreign dir without .origin, a reserved name and the harness key are never claimed.
func TestForeignAndReservedDirsNeverClaimed(t *testing.T) {
	cfg := tempConfig(t)
	c := gitRepo(t, t.TempDir(), "zed")
	os.MkdirAll(filepath.Join(cfg, "memory", "zed"), 0o755)
	if dc := mustRepoDir(t, c); !strings.HasPrefix(filepath.Base(dc), "zed--") {
		t.Errorf("foreign dir claimed: %s", dc)
	}
	for name := range maps.Keys(storeReservedDirs) {
		r := gitRepo(t, t.TempDir(), name)
		if dr := mustRepoDir(t, r); filepath.Base(dr) == name {
			t.Errorf("reserved key %q claimed", name)
		}
	}
}

func TestSlugKeyExtendsOnHashCollision(t *testing.T) {
	t.Parallel()
	taken := func(key string) (Origin, bool) {
		switch {
		case key == "x":
			return Origin{Origin: "/other"}, true
		case len(key) == len("x--")+8:
			return Origin{Origin: "/other2"}, true
		}
		return Origin{}, false
	}
	key, err := slugKey("x", "/me", taken)
	if err != nil || !strings.HasPrefix(key, "x--") || len(key) != len("x--")+12 {
		t.Errorf("slugKey = %q, %v", key, err)
	}
	always := func(string) (Origin, bool) { return Origin{Origin: "/other"}, true }
	if _, err := slugKey("x", "/me", always); err == nil {
		t.Error("exhausted hash must error, not overwrite")
	}
	if key, _ := slugKey("..", "/me", func(string) (Origin, bool) { return Origin{}, false }); key != "repo" {
		t.Errorf("degenerate base = %q", key)
	}
}

func TestAliasRoutesAndConflicts(t *testing.T) {
	tempConfig(t)
	a := gitRepo(t, t.TempDir(), "alpha")
	b := gitRepo(t, t.TempDir(), "beta")
	c := gitRepo(t, t.TempDir(), "gamma")
	da, _ := EnsureRepoDir(a)
	db, _ := EnsureRepoDir(b)
	if err := addOriginAlias(da, c); err != nil {
		t.Fatal(err)
	}
	if err := addOriginAlias(da, c); err != nil {
		t.Fatal("alias add must be idempotent:", err)
	}
	if got := mustRepoDir(t, c); got != da {
		t.Errorf("RepoDir(aliased) = %s want %s", got, da)
	}
	if o, _ := ReadOrigin(da); len(o.Aliases) != 1 || o.Aliases[0] != paths.CanonDeep(c) || o.Origin != paths.CanonDeep(a) {
		t.Errorf("origin after alias = %+v", o)
	}
	if err := addOriginAlias(db, c); !errors.Is(err, errOriginConflict) {
		t.Errorf("second claim must conflict, got %v", err)
	}
	if err := addOriginAlias(db, a); !errors.Is(err, errOriginConflict) {
		t.Errorf("claiming another store's origin must conflict, got %v", err)
	}
	if err := addOriginAlias(da, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("alias must exist on disk")
	}
}

func TestOriginReadWriteGuards(t *testing.T) {
	cfg := tempConfig(t)
	dir := filepath.Join(cfg, "memory", "one")
	os.MkdirAll(dir, 0o755)
	o := Origin{Version: OriginVersion, Key: "one", Origin: "/src/one", Aliases: []string{"", "/x/../y", "/y"}}
	if err := WriteOrigin(dir, o); err != nil {
		t.Fatal(err)
	}
	got, err := ReadOrigin(dir)
	if err != nil || got.Origin != "/src/one" || len(got.Aliases) != 1 || got.Aliases[0] != "/y" {
		t.Fatalf("ReadOrigin = %+v, %v", got, err)
	}
	if err := WriteOrigin(dir, o); err != nil {
		t.Error("same identity must be a no-op:", err)
	}
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: "one", Origin: "/src/other"}); !errors.Is(err, errOriginConflict) {
		t.Errorf("different origin must conflict, got %v", err)
	}
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: "two", Origin: "/src/one"}); err == nil {
		t.Error("key must match the directory")
	}
}

func TestReadOriginRejectsSymlinkedStore(t *testing.T) {
	cfg := tempConfig(t)
	dir := filepath.Join(cfg, "memory", "one")
	os.MkdirAll(dir, 0o755)
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: "one", Origin: "/src/one"}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg, "memory", "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	if _, err := ReadOrigin(link); err == nil {
		t.Error("symlinked store must be rejected")
	}
}

// TestResolveIgnoresInheritedGitEnv proves a hostile GIT_DIR/GIT_WORK_TREE pointed at a decoy repo
// cannot redirect identity resolution of a real repo — runGit strips every inherited GIT_* var.
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

// TestAliasToHarnessStore matches the plan's required case: a hand-written .claude/.origin aliasing
// an external checkout (~/other/claude-code) must route that checkout's RepoDir to the harness store.
func TestAliasToHarnessStore(t *testing.T) {
	cfg := gitRepo(t, t.TempDir(), ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	ext := gitRepo(t, filepath.Join(t.TempDir(), "ext"), "claude-code")
	dir := harnessStoreDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := Origin{Version: OriginVersion, Key: harnessStoreKey(), Origin: paths.CanonDeep(cfg), Aliases: []string{paths.CanonDeep(ext)}}
	if err := WriteOrigin(dir, o); err != nil {
		t.Fatal(err)
	}
	if got := mustRepoDir(t, ext); got != dir {
		t.Errorf("RepoDir(ext alias) = %s want %s", got, dir)
	}
	// addOriginAlias must refuse to hand that same origin to a different store.
	other := gitRepo(t, t.TempDir(), "other")
	odir, err := EnsureRepoDir(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := addOriginAlias(odir, ext); !errors.Is(err, errOriginConflict) {
		t.Errorf("claiming the harness's alias from another store must conflict, got %v", err)
	}
}

// TestWorkspaceOriginResolvesNonGitDirAndSubdirs: a store whose .origin names a non-git directory
// resolves that directory AND its subdirs to the workspace (never HomeBank); an unrelated non-git dir
// has no matching store and stays HomeBank.
func TestWorkspaceOriginResolvesNonGitDirAndSubdirs(t *testing.T) {
	cfg := tempConfig(t)
	t.Setenv("HOME", t.TempDir())
	raw := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(raw, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := paths.CanonDeep(raw)
	dir := filepath.Join(cfg, "memory", "ws")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: "ws", Origin: workspace}); err != nil {
		t.Fatal(err)
	}

	sub := filepath.Join(workspace, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{workspace, sub} {
		assertWorkspaceRoot(t, root, workspace, dir)
	}

	unrelated := t.TempDir()
	if id := Resolve(unrelated); !id.HomeBank || id.Origin != "" {
		t.Errorf("Resolve(unrelated non-git dir) = %+v, want HomeBank", id)
	}
}

// assertWorkspaceRoot checks root resolves to the workspace origin and its store dir.
func assertWorkspaceRoot(t *testing.T, root, workspace, dir string) {
	t.Helper()
	id := Resolve(root)
	if id.HomeBank || id.Origin != workspace {
		t.Errorf("Resolve(%s) = %+v, want the workspace origin %s", root, id, workspace)
	}
	if got, err := RepoDir(root); err != nil || got != dir {
		t.Errorf("RepoDir(%s) = %s, %v, want %s", root, got, err, dir)
	}
}

// seedJunkStores fills store with two valid repos and every shape RepoDirs must skip.
func seedJunkStores(t *testing.T, store string) {
	t.Helper()
	for _, k := range []string{"zeta", "alpha"} {
		os.MkdirAll(filepath.Join(store, k), 0o755)
		if err := WriteOrigin(filepath.Join(store, k), Origin{Version: OriginVersion, Key: k, Origin: "/src/" + k}); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(store, "noorigin"), 0o755)
	write(t, filepath.Join(store, "file.md"), "x")
	os.MkdirAll(filepath.Join(store, "mismatch"), 0o755)
	write(t, filepath.Join(store, "mismatch", legacyOriginFile), `{"version":1,"key":"other","origin":"/src/other"}`)
	for r := range maps.Keys(storeReservedDirs) {
		os.MkdirAll(filepath.Join(store, r), 0o755)
		write(t, filepath.Join(store, r, legacyOriginFile), `{"version":1,"key":"`+r+`","origin":"/r"}`)
	}
	os.Symlink(filepath.Join(store, "alpha"), filepath.Join(store, "sym"))
}

func TestRepoDirsSkipsJunk(t *testing.T) {
	cfg := tempConfig(t)
	store := filepath.Join(cfg, "memory")
	if got, err := RepoDirs(); got != nil || err != nil {
		t.Fatalf("missing store = %v, %v", got, err)
	}
	seedJunkStores(t, store)
	got, err := RepoDirs()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range got {
		keys = append(keys, e.Key)
		if e.Dir != filepath.Join(store, e.Key) || e.Origin.Origin != "/src/"+e.Key {
			t.Errorf("entry %+v", e)
		}
	}
	if strings.Join(keys, ",") != "alpha,zeta" {
		t.Errorf("keys = %v", keys)
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
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "x")
	}
}

// resetRemoteCache drops the memoized remotes.
func resetRemoteCache() { remoteCache.Clear() }

// TestOriginRemoteURLIsMemoized: the second lookup for one checkout (hit or miss) forks no git; the reset hook clears it.
func TestOriginRemoteURLIsMemoized(t *testing.T) {
	tempConfig(t)
	withRemote := gitRepo(t, t.TempDir(), "has-remote")
	fixtureGit(t, withRemote, "remote", "add", "origin", "https://github.com/tester/memo.git")
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

	fixtureGit(t, noRemote, "remote", "add", "origin", "https://github.com/tester/later.git")
	if got := originRemoteURL(noRemote); got != "" {
		t.Fatalf("cache should hide the new remote, got %q", got)
	}
	resetRemoteCache()
	if got := originRemoteURL(noRemote); got != "https://github.com/tester/later.git" {
		t.Fatalf("after reset, originRemoteURL = %q, want the new remote", got)
	}
}

// TestRepoKeyFromRemote pins the basename extraction rule (spec: store re-layout §1) across URL shapes.
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

// TestRepoKeyUsesOriginRemote: a checkout whose origin remote basename differs from its dir name is keyed
// by the remote, not the dir — the dir name must never leak into the store key when a remote exists.
func TestRepoKeyUsesOriginRemote(t *testing.T) {
	cfg := tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "mismatched-dirname")
	fixtureGit(t, repo, "remote", "add", "origin", "https://github.com/tester/claude-dbx.git")

	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cfg, "memory", "claude-dbx")
	if dir != want {
		t.Fatalf("EnsureRepoDir(remote-keyed repo) = %s, want %s", dir, want)
	}
	o, err := ReadOrigin(dir)
	if err != nil || o.Key != "claude-dbx" {
		t.Fatalf("origin = %+v, %v", o, err)
	}
}

// TestRepoKeyFallsBackToDirBasenameWithoutRemote: no origin remote keeps the pre-spec behaviour.
func TestRepoKeyFallsBackToDirBasenameWithoutRemote(t *testing.T) {
	cfg := tempConfig(t)
	repo := gitRepo(t, t.TempDir(), "no-remote-repo")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "memory", "no-remote-repo"); dir != want {
		t.Fatalf("EnsureRepoDir(no-remote repo) = %s, want %s", dir, want)
	}
}

// TestSameRemoteDifferentCheckoutsDoNotShareAStore: two distinct clones (different checkout paths) that
// share one origin remote URL resolve to the same slug, so the second must take a hash-suffixed key
// (existing takenKeys/slugKey collision behaviour) rather than silently writing into the first's store.
// assertCollisionKey checks db is the hash-suffixed store dir the collision logic derives from checkout.
func assertCollisionKey(t *testing.T, db, checkout string) {
	t.Helper()
	sum := sha256.Sum256([]byte(paths.CanonDeep(checkout)))
	wantKey := "collide--" + hex.EncodeToString(sum[:])[:8]
	if got := filepath.Base(db); got != wantKey {
		t.Fatalf("second clone's key = %q, want %q (exact key the collision logic produced)", got, wantKey)
	}
	if !strings.HasPrefix(filepath.Base(db), "collide--") {
		t.Fatalf("second clone's key %q lost the collision suffix", filepath.Base(db))
	}
}

func TestSameRemoteDifferentCheckoutsDoNotShareAStore(t *testing.T) {
	cfg := tempConfig(t)
	const remote = "https://github.com/tester/collide.git"
	a := gitRepo(t, t.TempDir(), "clone-a")
	fixtureGit(t, a, "remote", "add", "origin", remote)
	b := gitRepo(t, t.TempDir(), "clone-b")
	fixtureGit(t, b, "remote", "add", "origin", remote)

	da, err := EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cfg, "memory", "collide"); da != want {
		t.Fatalf("first clone's dir = %s, want %s", da, want)
	}

	db, err := EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	if db == da {
		t.Fatal("second clone silently wrote into the first clone's store")
	}
	assertCollisionKey(t, db, b)

	assertBothStoresFindable(t, a, b, da, db)
}

// Both stores remain independently findable, and the first's .origin was never rewritten.
func assertBothStoresFindable(t *testing.T, a, b, da, db string) {
	t.Helper()
	if mustRepoDir(t, a) != da || mustRepoDir(t, b) != db {
		t.Error("RepoDir must find both stores by their own checkout path")
	}
	oa, err := ReadOrigin(da)
	if err != nil || oa.Origin != paths.CanonDeep(a) {
		t.Fatalf("first store's origin was clobbered: %+v, %v", oa, err)
	}
	ob, err := ReadOrigin(db)
	if err != nil || ob.Origin != paths.CanonDeep(b) {
		t.Fatalf("second store's origin = %+v, %v, want %s", ob, err, paths.CanonDeep(b))
	}
}
