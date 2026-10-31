package memory

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

import (
	"github.com/edotau/claude-code/internal/memstore"
)

// A cwd inside the store resolves to the repo its store dir describes, not to the store's own git repo ("memory").
func TestResolveInsideStore(t *testing.T) {
	cfg := gitRepo(t, t.TempDir(), ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	proj := gitRepo(t, t.TempDir(), "proj")
	if err := (Repo{Dir: memstore.StoreDir()}).Ensure(); err != nil {
		t.Fatal(err)
	}
	harness, err := memstore.EnsureRepoDir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	projStore, err := memstore.EnsureRepoDir(proj)
	if err != nil {
		t.Fatal(err)
	}
	archive := memstore.ArchiveDir(harness)
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	if id := memstore.Resolve(memstore.StoreDir()); !id.HomeBank {
		t.Errorf("Resolve(store top) = %+v, want global", id)
	}
	for _, root := range []string{harness, archive} {
		if id := memstore.Resolve(root); !id.Harness {
			t.Errorf("Resolve(%s) = %+v, want harness", root, id)
		}
	}
	if id := memstore.Resolve(projStore); id.Origin != memstore.ResolvePath(proj) {
		t.Errorf("Resolve(%s) = %+v, want origin %s", projStore, id, proj)
	}
}

// TestBuildCatalogRebuildsIdenticalRowsAfterVaultLoss: BuildCatalog's rows (with their origin
// comments) are the sole durable record once the vault is gone — deleting vault.db and asking
// RepoDirs again must reproduce exactly the same origins from the catalog it wrote.
func TestBuildCatalogRebuildsIdenticalRowsAfterVaultLoss(t *testing.T) {
	cfg := tempConfig(t)
	for _, k := range []string{"alpha", "beta"} {
		dir := filepath.Join(cfg, "memory", k)
		os.MkdirAll(dir, 0o755)
		if err := memstore.WriteOrigin(dir, memstore.Origin{Version: memstore.OriginVersion, Key: k, Origin: "/src/" + k, Aliases: []string{"/alt-" + k}}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := memstore.RepoDirs()
	if err != nil || len(before) != 2 {
		t.Fatalf("RepoDirs before = %+v, %v", before, err)
	}
	catalog, err := BuildCatalog(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(cfg, "memory", memstore.CatalogFile), catalog)

	if err := os.Remove(vault.Path()); err != nil {
		t.Fatal(err)
	}
	after, err := memstore.RepoDirs()
	if err != nil || len(after) != 2 {
		t.Fatalf("RepoDirs after vault loss = %+v, %v", after, err)
	}
	for i := range before {
		if !reflect.DeepEqual(before[i].Origin, after[i].Origin) {
			t.Errorf("entry %d origin changed: before %+v, after %+v", i, before[i].Origin, after[i].Origin)
		}
	}
}

// TestAliasFoldDeletesSourceVaultRow: AddOriginAlias-style folding (FoldOrigin's drop path) must
// delete the source store's own vault row in the same transaction that gives the target the alias —
// never leave a stale row behind for a dir that no longer exists.
func TestAliasFoldDeletesSourceVaultRow(t *testing.T) {
	e := newAliasEnv(t)
	if _, err := alias(t, e, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := vaultGet(t, e.kb); ok {
		t.Errorf("source key %q still has a vault row after folding into %q", e.kb, e.ka)
	}
	got, err := memstore.ReadOrigin(e.da)
	if err != nil || !got.Accepts(e.b) {
		t.Errorf("target row missing the folded alias: %+v, %v", got, err)
	}
}

// TestLinkNativeForUsesMainCheckoutAndTypedPath: a worktree or a subdir resolves to the main checkout's
// link; a non-git cwd gets its own typed path pointed at the global tier.
func TestLinkNativeForUsesMainCheckoutAndTypedPath(t *testing.T) {
	isolatedConfig(t)
	initStore(t)

	repo := gitRepo(t, canonTemp(t, t.TempDir()), "proj")
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(canonTemp(t, t.TempDir()), "wt")
	fixtureGit(t, repo, "worktree", "add", "-q", wt)
	wtSub := filepath.Join(wt, "a")
	if err := os.MkdirAll(wtSub, 0o755); err != nil {
		t.Fatal(err)
	}

	dir, err := memstore.EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	wantLink := memstore.ProjectsMemory(memstore.ProjectSlug(repo))
	for _, cwd := range []string{repo, sub, wt, wtSub} {
		res, ok := memstore.LinkNativeFor(cwd)
		if !ok {
			t.Fatalf("LinkNativeFor(%s) ok = false", cwd)
		}
		if res.Target != dir || res.Link != wantLink {
			t.Errorf("LinkNativeFor(%s) = %+v, want target %s link %s", cwd, res, dir, wantLink)
		}
	}

	nonGit := t.TempDir()
	// LinkNativeFor refuses to link into a store dir that doesn't exist yet (same guard the repo case
	// above relies on EnsureRepoDir for); the global tier now lives in its own subdir, so seed it too.
	if err := os.MkdirAll(memstore.HomeBankDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	res, ok := memstore.LinkNativeFor(nonGit)
	if !ok {
		t.Fatal("a non-git cwd must still resolve (to the global tier)")
	}
	if res.Link != memstore.ProjectsMemory(memstore.ProjectSlug(nonGit)) || res.Target != memstore.HomeBankDir() {
		t.Errorf("non-git LinkNativeFor = %+v, want its own typed path pointed at the global tier", res)
	}
}
