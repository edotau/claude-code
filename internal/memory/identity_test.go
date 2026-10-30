package memory

import (
	"github.com/edotau/claude-code/internal/hosttest"

	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

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

// gitRepo makes a real disposable repository with one commit at <parent>/<name>, copied from a once-built template.
func gitRepo(t *testing.T, parent, name string) string {
	t.Helper()
	root := filepath.Join(parent, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	tmpl := gitRepoTemplate(t)
	if out, err := exec.Command("cp", "-a", filepath.Join(tmpl, ".git"), root).CombinedOutput(); err != nil {
		t.Fatalf("copy template repo: %v %s", err, out)
	}
	return root
}

var (
	gitTmplOnce sync.Once
	gitTmplDir  string
)

func gitRepoTemplate(t *testing.T) string {
	t.Helper()
	gitTmplOnce.Do(func() {
		dir, err := os.MkdirTemp("", "memory-gittmpl-")
		if err != nil {
			t.Fatal(err)
		}
		gitTmplDir = dir
		fixtureGit(t, dir, "init", "-q", "-b", "main")
		fixtureGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	})
	if gitTmplDir == "" {
		t.Fatal("git template repo unavailable")
	}
	return gitTmplDir
}

func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	code := m.Run()
	cleanup()
	if gitTmplDir != "" {
		os.RemoveAll(gitTmplDir)
	}
	os.Exit(code)
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
	if want.HomeBank || want.Harness || want.Base != "proj" || want.Origin != resolvePath(repo) {
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

// A cwd inside the store resolves to the repo its store dir describes, not to the store's own git repo ("memory").
func TestResolveInsideStore(t *testing.T) {
	cfg := gitRepo(t, t.TempDir(), ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	proj := gitRepo(t, t.TempDir(), "proj")
	if err := (Repo{Dir: StoreDir()}).Ensure(); err != nil {
		t.Fatal(err)
	}
	harness, err := EnsureRepoDir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	projStore, err := EnsureRepoDir(proj)
	if err != nil {
		t.Fatal(err)
	}
	archive := ArchiveDir(harness)
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	if id := Resolve(StoreDir()); !id.HomeBank {
		t.Errorf("Resolve(store top) = %+v, want global", id)
	}
	for _, root := range []string{harness, archive} {
		if id := Resolve(root); !id.Harness {
			t.Errorf("Resolve(%s) = %+v, want harness", root, id)
		}
	}
	if id := Resolve(projStore); id.Origin != resolvePath(proj) {
		t.Errorf("Resolve(%s) = %+v, want origin %s", projStore, id, proj)
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
		id := Resolve(root)
		if !id.Harness || id.HomeBank || id.Origin != resolvePath(cfg) {
			t.Errorf("Resolve(%s) = %+v", root, id)
		}
		if dir, err := RepoDir(root); err != nil || dir != HarnessStoreDir() {
			t.Errorf("RepoDir(%s) = %s, %v", root, dir, err)
		}
	}
	dir, err := EnsureRepoDir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	o, err := ReadOrigin(dir)
	if err != nil || o.Key != harnessStoreKey() || o.Origin != resolvePath(cfg) {
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
	if got, err := RepoDir(worktree); err != nil || got != HarnessStoreDir() {
		t.Errorf("RepoDir(worktree) = %q, %v, want %q", got, err, HarnessStoreDir())
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

func TestRepoDirReadOnlyAndEnsureIdempotent(t *testing.T) {
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
	got, err := EnsureRepoDir(repo)
	if err != nil || got != dir {
		t.Fatalf("EnsureRepoDir = %s, %v", got, err)
	}
	o, err := ReadOrigin(dir)
	if err != nil || o.Origin != resolvePath(repo) || o.Key != "my-repo.v2" {
		t.Fatalf("origin = %+v, %v", o, err)
	}
	if again, err := EnsureRepoDir(repo); err != nil || again != dir {
		t.Fatal(err)
	}
	if after, err := ReadOrigin(dir); err != nil || !reflect.DeepEqual(after, o) {
		t.Errorf("second EnsureRepoDir rewrote the origin record: %+v, %v", after, err)
	}
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
	if oa, _ := ReadOrigin(da); oa.Origin != resolvePath(a) {
		t.Errorf("first store's origin was clobbered: %+v", oa)
	}
	// A foreign dir without .origin, a reserved name and the harness key are never claimed.
	c := gitRepo(t, t.TempDir(), "zed")
	os.MkdirAll(filepath.Join(cfg, "memory", "zed"), 0o755)
	if dc := mustRepoDir(t, c); !strings.HasPrefix(filepath.Base(dc), "zed--") {
		t.Errorf("foreign dir claimed: %s", dc)
	}
	for name := range maps.Keys(StoreReservedDirs) {
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
	if err := AddOriginAlias(da, c); err != nil {
		t.Fatal(err)
	}
	if err := AddOriginAlias(da, c); err != nil {
		t.Fatal("alias add must be idempotent:", err)
	}
	if got := mustRepoDir(t, c); got != da {
		t.Errorf("RepoDir(aliased) = %s want %s", got, da)
	}
	if o, _ := ReadOrigin(da); len(o.Aliases) != 1 || o.Aliases[0] != resolvePath(c) || o.Origin != resolvePath(a) {
		t.Errorf("origin after alias = %+v", o)
	}
	if err := AddOriginAlias(db, c); !errors.Is(err, ErrOriginConflict) {
		t.Errorf("second claim must conflict, got %v", err)
	}
	if err := AddOriginAlias(db, a); !errors.Is(err, ErrOriginConflict) {
		t.Errorf("claiming another store's origin must conflict, got %v", err)
	}
	if err := AddOriginAlias(da, filepath.Join(t.TempDir(), "missing")); err == nil {
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
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: "one", Origin: "/src/other"}); !errors.Is(err, ErrOriginConflict) {
		t.Errorf("different origin must conflict, got %v", err)
	}
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: "two", Origin: "/src/one"}); err == nil {
		t.Error("key must match the directory")
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
	if got := Resolve(a); got.Base != "proj" || got.Origin != resolvePath(a) {
		t.Errorf("Resolve under hostile GIT_DIR/GIT_WORK_TREE = %+v, want proj", got)
	}
}

// TestAliasToHarnessStore matches the plan's required case: a hand-written .claude/.origin aliasing
// an external checkout (~/orca/claude-code) must route that checkout's RepoDir to the harness store.
func TestAliasToHarnessStore(t *testing.T) {
	cfg := gitRepo(t, t.TempDir(), ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	orca := gitRepo(t, filepath.Join(t.TempDir(), "orca"), "claude-code")
	dir := HarnessStoreDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := Origin{Version: OriginVersion, Key: harnessStoreKey(), Origin: resolvePath(cfg), Aliases: []string{resolvePath(orca)}}
	if err := WriteOrigin(dir, o); err != nil {
		t.Fatal(err)
	}
	if got := mustRepoDir(t, orca); got != dir {
		t.Errorf("RepoDir(orca alias) = %s want %s", got, dir)
	}
	// AddOriginAlias must refuse to hand that same origin to a different store.
	other := gitRepo(t, t.TempDir(), "other")
	odir, err := EnsureRepoDir(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddOriginAlias(odir, orca); !errors.Is(err, ErrOriginConflict) {
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
	workspace := resolvePath(raw)
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
		id := Resolve(root)
		if id.HomeBank || id.Origin != workspace {
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

func TestRepoDirsSkipsJunk(t *testing.T) {
	cfg := tempConfig(t)
	store := filepath.Join(cfg, "memory")
	if got, err := RepoDirs(); got != nil || err != nil {
		t.Fatalf("missing store = %v, %v", got, err)
	}
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
	for r := range maps.Keys(StoreReservedDirs) {
		os.MkdirAll(filepath.Join(store, r), 0o755)
		write(t, filepath.Join(store, r, legacyOriginFile), `{"version":1,"key":"`+r+`","origin":"/r"}`)
	}
	os.Symlink(filepath.Join(store, "alpha"), filepath.Join(store, "sym"))
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
