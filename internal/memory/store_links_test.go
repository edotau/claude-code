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
func wantAction(t *testing.T, label string, r LinkResult, want string) {
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

// TestLinkNativeForNoopBeforeCutover: LinkNativeFor reports ok=false before the migration lands.
func TestLinkNativeForNoopBeforeCutover(t *testing.T) {
	isolatedConfig(t)
	if _, ok := LinkNativeFor(t.TempDir()); ok {
		t.Error("LinkNativeFor before cutover must report ok=false")
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
