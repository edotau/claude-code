package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

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

// TestEnsureLinkStates drives every branch of ensureLink: absent, a correct link, a wrong link, an empty
// real dir, and a non-empty real dir.
func TestEnsureLinkStates(t *testing.T) {
	t.Parallel()
	cfg := canonTemp(t, t.TempDir())
	target := filepath.Join(cfg, "memory", "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfg, "projects", "proj", "memory")

	// absent -> linked
	if r := ensureLink(link, target); r.Action != "linked" {
		t.Fatalf("absent: %+v", r)
	}
	assertLinkedTo(t, link, target)

	// correct link -> ok
	if r := ensureLink(link, target); r.Action != "ok" {
		t.Fatalf("existing correct link: %+v", r)
	}

	// wrong link -> relinked
	other := filepath.Join(cfg, "memory", "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	r := ensureLink(link, other)
	if r.Action != "relinked" || !strings.Contains(r.Detail, target) {
		t.Fatalf("wrong link: %+v", r)
	}
	assertLinkedTo(t, link, other)

	// reset to a real, empty dir -> replaced by a link
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if r := ensureLink(link, target); r.Action != "linked" {
		t.Fatalf("empty real dir: %+v", r)
	}
	assertLinkedTo(t, link, target)

	// reset to a real, non-empty dir -> blocked, content untouched
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	content := filepath.Join(link, "note.md")
	write(t, content, "real content\n")
	r = ensureLink(link, target)
	if r.Action != "blocked" {
		t.Fatalf("non-empty dir: %+v", r)
	}
	if got := readFile(t, content); got != "real content\n" {
		t.Errorf("content touched by a blocked link: %q", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Error("a blocked non-empty dir must stay a real directory")
	}
}

// TestEnsureNativeLinksNoopBeforeCutover: EnsureNativeLinks is a no-op (nil, nil) before the migration lands.
func TestEnsureNativeLinksNoopBeforeCutover(t *testing.T) {
	migrateConfig1(t)
	out, err := EnsureNativeLinks()
	if err != nil || out != nil {
		t.Fatalf("before cutover: out=%v err=%v", out, err)
	}
}

// TestEnsureNativeLinksAfterCutoverCoversHomeAndRepoAliases: after cutover every known project's native
// memory dir is a symlink into its store — $HOME into HomeBankDir(), a repo and its alias both into the
// same repo store dir.
func TestEnsureNativeLinksAfterCutoverCoversHomeAndRepoAliases(t *testing.T) {
	_, home := migrateConfig(t)
	cutover(t)

	repo := gitRepo(t, canonTemp(t, t.TempDir()), "proj")
	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	alias := gitRepo(t, canonTemp(t, t.TempDir()), "alias-of-proj")
	if err := AddOriginAlias(dir, alias); err != nil {
		t.Fatal(err)
	}

	out, err := EnsureNativeLinks()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("results = %+v, want 3 (home, repo, alias)", out)
	}
	assertLinkedTo(t, projectsMemory(projectSlug(home)), HomeBankDir())
	assertLinkedTo(t, projectsMemory(projectSlug(repo)), dir)
	assertLinkedTo(t, projectsMemory(projectSlug(alias)), dir)

	// idempotent re-run: every result is already "ok"
	out2, err := EnsureNativeLinks()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range out2 {
		if r.Action != "ok" {
			t.Errorf("re-run result = %+v, want ok", r)
		}
	}
}

// TestLinkNativeForUsesMainCheckoutAndTypedPath: a worktree or a subdir resolves to the main checkout's
// link; a non-git cwd gets its own typed path pointed at the global tier.
func TestLinkNativeForUsesMainCheckoutAndTypedPath(t *testing.T) {
	migrateConfig(t)
	cutover(t)

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

	dir, err := EnsureRepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	wantLink := projectsMemory(projectSlug(repo))
	for _, cwd := range []string{repo, sub, wt, wtSub} {
		res, ok := LinkNativeFor(cwd)
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
	if err := os.MkdirAll(HomeBankDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	res, ok := LinkNativeFor(nonGit)
	if !ok {
		t.Fatal("a non-git cwd must still resolve (to the global tier)")
	}
	if res.Link != projectsMemory(projectSlug(nonGit)) || res.Target != HomeBankDir() {
		t.Errorf("non-git LinkNativeFor = %+v, want its own typed path pointed at the global tier", res)
	}
}

// TestLinkNativeForNoopBeforeCutover: LinkNativeFor reports ok=false before the migration lands.
func TestLinkNativeForNoopBeforeCutover(t *testing.T) {
	migrateConfig1(t)
	if _, ok := LinkNativeFor(t.TempDir()); ok {
		t.Error("LinkNativeFor before cutover must report ok=false")
	}
}

// migrateConfig1 isolates CLAUDE_CONFIG_DIR and HOME like migrateConfig but returns only cfg, for
// tests that never need the home path itself.
func migrateConfig1(t *testing.T) string {
	cfg, _ := migrateConfig(t)
	return cfg
}
