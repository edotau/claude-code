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
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// reset drops the memo; tests call it after rewriting the vault or catalog by hand.
func (m *catalogMemo) reset() { m.store("", nil) }

func idFatal(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func idEnsure(t *testing.T, root string) string {
	t.Helper()
	dir, err := EnsureRepoDir(root)
	idFatal(t, err)
	return dir
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
		if dir, err := RepoDir(root); err != nil || dir != harnessStoreDir() {
			t.Errorf("RepoDir(%s) = %s, %v", root, dir, err)
		}
	}
	dir := idEnsure(t, cfg)
	o, err := readOrigin(dir)
	if err != nil || o.Key != harnessStoreKey() || o.Origin != paths.CanonDeep(cfg) {
		t.Errorf("harness .origin = %+v, %v", o, err)
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

// The config dir is itself a linked worktree of another checkout.
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
	if plain := t.TempDir(); mustRepoDir(t, plain) != HomeBankDir() {
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
	o, err := readOrigin(dir)
	if err != nil || o.Origin != paths.CanonDeep(repo) || o.Key != "my-repo.v2" {
		t.Fatalf("origin = %+v, %v", o, err)
	}
	if again, err := EnsureRepoDir(repo); err != nil || again != dir {
		t.Fatal(err)
	}
	if after, err := readOrigin(dir); err != nil || !reflect.DeepEqual(after, o) {
		t.Errorf("second EnsureRepoDir rewrote the origin record: %+v, %v", after, err)
	}
	if g, err := EnsureRepoDir(t.TempDir()); err != nil || g != HomeBankDir() {
		t.Errorf("global EnsureRepoDir = %s, %v", g, err)
	}
	if _, err := readOrigin(HomeBankDir()); err == nil {
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
	da := idEnsure(t, a)
	db := idEnsure(t, b)
	if da != filepath.Join(cfg, "memory", "app") || !strings.HasPrefix(filepath.Base(db), "app--") || len(filepath.Base(db)) != len("app--")+8 {
		t.Fatalf("dirs = %s, %s", da, db)
	}
	if mustRepoDir(t, a) != da || mustRepoDir(t, b) != db {
		t.Error("RepoDir must find both stores by origin")
	}
	if oa, _ := readOrigin(da); oa.Origin != paths.CanonDeep(a) {
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
	idFatal(t, addOriginAlias(da, c))
	if err := addOriginAlias(da, c); err != nil {
		t.Fatal("alias add must be idempotent:", err)
	}
	if got := mustRepoDir(t, c); got != da {
		t.Errorf("RepoDir(aliased) = %s want %s", got, da)
	}
	if o, _ := readOrigin(da); len(o.Aliases) != 1 || o.Aliases[0] != paths.CanonDeep(c) || o.Origin != paths.CanonDeep(a) {
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
	idFatal(t, WriteOrigin(dir, o))
	got, err := readOrigin(dir)
	if err != nil || got.Origin != "/src/one" || len(got.Aliases) != 1 || got.Aliases[0] != "/y" {
		t.Fatalf("readOrigin = %+v, %v", got, err)
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
	idFatal(t, WriteOrigin(dir, Origin{Version: OriginVersion, Key: "one", Origin: "/src/one"}))
	link := filepath.Join(cfg, "memory", "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	if _, err := readOrigin(link); err == nil {
		t.Error("symlinked store must be rejected")
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

// A hand-written harness .origin aliasing an external checkout routes that checkout to the harness store.
func TestAliasToHarnessStore(t *testing.T) {
	cfg := gitRepo(t, t.TempDir(), ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	ext := gitRepo(t, filepath.Join(t.TempDir(), "ext"), "claude-code")
	dir := harnessStoreDir()
	idMkdir(t, dir)
	o := Origin{Version: OriginVersion, Key: harnessStoreKey(), Origin: paths.CanonDeep(cfg), Aliases: []string{paths.CanonDeep(ext)}}
	idFatal(t, WriteOrigin(dir, o))
	if got := mustRepoDir(t, ext); got != dir {
		t.Errorf("RepoDir(ext alias) = %s want %s", got, dir)
	}
	// addOriginAlias must refuse to hand that same origin to a different store.
	other := gitRepo(t, t.TempDir(), "other")
	odir := idEnsure(t, other)
	if err := addOriginAlias(odir, ext); !errors.Is(err, errOriginConflict) {
		t.Errorf("claiming the harness's alias from another store must conflict, got %v", err)
	}
}

// A store whose .origin names a non-git dir claims it and its subdirs; an unrelated non-git dir stays HomeBank.
func TestWorkspaceOriginResolvesNonGitDirAndSubdirs(t *testing.T) {
	cfg := tempConfig(t)
	t.Setenv("HOME", t.TempDir())
	raw := filepath.Join(t.TempDir(), "workspace")
	idMkdir(t, raw)
	workspace := paths.CanonDeep(raw)
	dir := filepath.Join(cfg, "memory", "ws")
	idMkdir(t, dir)
	idFatal(t, WriteOrigin(dir, Origin{Version: OriginVersion, Key: "ws", Origin: workspace}))
	sub := filepath.Join(workspace, "a", "b")
	idMkdir(t, sub)
	for _, root := range []string{workspace, sub} {
		if id := Resolve(root); id.HomeBank || id.Origin != workspace {
			t.Errorf("Resolve(%s) = %+v, want the workspace origin %s", root, id, workspace)
		}
		if got, err := RepoDir(root); err != nil || got != dir {
			t.Errorf("RepoDir(%s) = %s, %v, want %s", root, got, err, dir)
		}
	}
	unrelated := t.TempDir()
	if id := Resolve(unrelated); !id.HomeBank || id.Origin != "" {
		t.Errorf("Resolve(unrelated non-git dir) = %+v, want HomeBank", id)
	}
}

// seedJunkStores fills store with two valid repos and every shape RepoDirs must skip.
func seedJunkStores(t *testing.T, store string) {
	t.Helper()
	for _, k := range []string{"zeta", "alpha"} {
		os.MkdirAll(filepath.Join(store, k), 0o755)
		idFatal(t, WriteOrigin(filepath.Join(store, k), Origin{Version: OriginVersion, Key: k, Origin: "/src/" + k}))
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

// A settled catalog reads the vault once; a new store is never stale.
func TestRepoDirsReadsOriginsOncePerStamp(t *testing.T) {
	cfg := tempConfig(t)
	seedJunkStores(t, filepath.Join(cfg, "memory"))
	want, err := RepoDirs()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(vault.Path(), time.Time{}, time.Now().Add(-time.Hour)); err != nil { // past racyWindow
		t.Fatal(err)
	}
	reads, orig := 0, originRows
	originRows = func() (map[string]string, error) { reads++; return orig() }
	t.Cleanup(func() { originRows = orig; repoDirsMemo.reset() })
	for range 3 {
		if got, err := RepoDirs(); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("RepoDirs = %+v, %v, want %+v", got, err, want)
		}
	}
	if reads != 1 {
		t.Errorf("three RepoDirs on a settled catalog read the vault %d times, want 1", reads)
	}
	beta := filepath.Join(cfg, "memory", "beta")
	idFatal(t, errors.Join(os.Mkdir(beta, 0o755), WriteOrigin(beta, Origin{Version: OriginVersion, Key: "beta", Origin: "/src/beta"})))
	if got, _ := RepoDirs(); len(got) != len(want)+1 {
		t.Errorf("a store added mid-process was served stale: %+v", got)
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

// The store key comes from the origin remote's basename, never the dir name; a remote naming no repo falls back to the dir.
func TestRepoKeyFromOriginRemoteOrDir(t *testing.T) {
	for _, c := range []struct{ name, dirName, remote, key string }{
		{"remote beats dir name", "mismatched-dirname", "https://github.com/tester/claude-dbx.git", "claude-dbx"},
		{"bare slash remote", "no-remote-repo", "/", "no-remote-repo"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := tempConfig(t)
			repo := gitRepo(t, t.TempDir(), c.dirName)
			hosttest.In(t, repo, "remote", "add", "origin", c.remote)
			dir := idEnsure(t, repo)
			if want := filepath.Join(cfg, "memory", c.key); dir != want {
				t.Fatalf("EnsureRepoDir = %s, want %s", dir, want)
			}
			if o, err := readOrigin(dir); err != nil || o.Key != c.key {
				t.Fatalf("origin = %+v, %v", o, err)
			}
		})
	}
}

// assertCollisionKey checks db is the hash-suffixed store dir the collision logic derives from checkout.
func assertCollisionKey(t *testing.T, db, checkout string) {
	t.Helper()
	sum := sha256.Sum256([]byte(paths.CanonDeep(checkout)))
	wantKey := "collide--" + hex.EncodeToString(sum[:])[:8]
	if got := filepath.Base(db); got != wantKey {
		t.Fatalf("second clone's key = %q, want %q (exact key the collision logic produced)", got, wantKey)
	}
}

// Two clones sharing one remote URL: the second takes a hash-suffixed key, not the first's store.
func TestSameRemoteDifferentCheckoutsDoNotShareAStore(t *testing.T) {
	cfg := tempConfig(t)
	const remote = "https://github.com/tester/collide.git"
	a := gitRepo(t, t.TempDir(), "clone-a")
	hosttest.In(t, a, "remote", "add", "origin", remote)
	b := gitRepo(t, t.TempDir(), "clone-b")
	hosttest.In(t, b, "remote", "add", "origin", remote)
	da := idEnsure(t, a)
	if want := filepath.Join(cfg, "memory", "collide"); da != want {
		t.Fatalf("first clone's dir = %s, want %s", da, want)
	}
	db := idEnsure(t, b)
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
	oa, err := readOrigin(da)
	if err != nil || oa.Origin != paths.CanonDeep(a) {
		t.Fatalf("first store's origin was clobbered: %+v, %v", oa, err)
	}
	ob, err := readOrigin(db)
	if err != nil || ob.Origin != paths.CanonDeep(b) {
		t.Fatalf("second store's origin = %+v, %v, want %s", ob, err, paths.CanonDeep(b))
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

func seedRouteStore(t *testing.T, key, origin string) string {
	t.Helper()
	dir := filepath.Join(StoreDir(), key)
	idMkdir(t, dir)
	idFatal(t, WriteOrigin(dir, Origin{Version: OriginVersion, Key: key, Origin: origin}))
	return dir
}

func TestRepoByNamePrefersExactKey(t *testing.T) {
	tempConfig(t)
	resetVaultRetry(t)
	seedRouteStore(t, "alpha", "/src/bravo")
	want := seedRouteStore(t, "bravo", "/other/checkout")
	got, ok := repoByName("bravo")
	if !ok || got.Key != "bravo" || got.Dir != want || got.Origin.Origin != "/other/checkout" {
		t.Fatalf("repoByName(bravo) = %+v, %v; want exact key bravo at %s, not alpha's origin basename", got, ok, want)
	}
}

func TestRoutedReposMatchesDottedName(t *testing.T) {
	tempConfig(t)
	resetVaultRetry(t)
	want := seedRouteStore(t, ".project", "/src/.project")
	if got := routedRepos("check .project"); len(got) != 1 || got[0].Key != ".project" || got[0].Dir != want {
		t.Fatalf("routedRepos(check .project) = %+v; want exactly .project at %s", got, want)
	}
}

// A routed name must appear as a whole word; a substring of it, or a key under 4 chars, never matches.
func TestRoutedReposWholeWordMatchAndShortKeysIgnored(t *testing.T) {
	isolatedConfig1(t)
	initStore(t)
	repo := gitRepo(t, t.TempDir(), "tester.github.io")
	if _, err := EnsureRepoDir(repo); err != nil {
		t.Fatal(err)
	}
	short := gitRepo(t, t.TempDir(), "abc")
	if _, err := EnsureRepoDir(short); err != nil {
		t.Fatal(err)
	}
	got := routedRepos("check Tester.GitHub.io, then deploy")
	if len(got) != 1 || filepath.Base(got[0].Origin.Origin) != "tester.github.io" {
		t.Fatalf("got = %+v, want exactly the tester.github.io entry", got)
	}
	for _, q := range []string{"my-tester.github.io", "tester.github.ion", "just github alone", "abc is here"} {
		if got := routedRepos(q); len(got) != 0 {
			t.Errorf("routedRepos(%q) = %+v; want no match (name inside a longer word, or a key under 4 chars)", q, got)
		}
	}
}

// repoByName folds case on the store key and, for a key that differs, on the origin's basename.
func TestRepoByNameMatchesKeyOrOriginBasename(t *testing.T) {
	tempConfig(t)
	resetVaultRetry(t)
	dir := seedRouteStore(t, "alpha", "/src/Checkout-Name")
	for _, name := range []string{"ALPHA", "checkout-name"} {
		if e, ok := repoByName(name); !ok || e.Dir != dir {
			t.Errorf("repoByName(%q) = %+v, %v; want the alpha store at %s", name, e, ok, dir)
		}
	}
	if e, ok := repoByName("src"); ok {
		t.Errorf("repoByName(src) matched %+v; only the origin's basename routes", e)
	}
}

const (
	lockedReadTimeout = 100 * time.Millisecond  // shrinks vault.View's lock wait for the test
	lockedReadBound   = 2500 * time.Millisecond // the ~2s vault.View timeout plus scheduling slack
)

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

// seedLockedCatalog writes an index.md row whose origin only the catalog can supply.
func seedLockedCatalog(t *testing.T) Origin {
	t.Helper()
	cfg := tempConfig(t)
	idMkdir(t, filepath.Join(cfg, "memory", "locked"))
	want := Origin{Version: OriginVersion, Key: "locked", Origin: "/src/locked"}
	row := "- locked — hook · last 2024-01-01 → locked/MEMORY.md" + catalogOriginComment(want) + "\n"
	write(t, filepath.Join(cfg, "memory", catalogFile), "# Memory catalog\n\n## Repos\n\n"+row)
	return want
}

// While a vault write txn holds bbolt's OS lock, RepoDirs reads index.md, then backs off.
func TestVaultLockedFallsBackToCatalogThenSkipsRetryFor30s(t *testing.T) {
	resetVaultRetry(t)
	t.Cleanup(vault.SetReadLockTimeoutForTest(lockedReadTimeout))
	want := seedLockedCatalog(t)
	err := vault.Update(func(*vault.Tx) error {
		start := time.Now()
		entries, err := RepoDirs()
		elapsed := time.Since(start)
		if err != nil || len(entries) != 1 || entries[0].Origin.Origin != want.Origin {
			t.Fatalf("RepoDirs under an external lock = %+v, %v, want the locked entry", entries, err)
		}
		if elapsed > lockedReadBound {
			t.Errorf("RepoDirs under lock took %v, want bounded by the ~2s vault.View timeout", elapsed)
		}

		start = time.Now()
		if _, err := RepoDirs(); err != nil {
			t.Fatal(err)
		}
		if again := time.Since(start); again > time.Second {
			t.Errorf("second call within the 30s backoff took %v, want near-instant (no repeat wait)", again)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// vault.View would block while locked, so the no-write check has to follow the release
	if _, ok := vaultGet(t, "locked"); ok {
		t.Error("a locked vault.View must never be followed by a write attempt")
	}
}
