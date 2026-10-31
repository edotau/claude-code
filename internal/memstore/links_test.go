package memstore

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

// TestLinkNativeForNoopBeforeCutover: LinkNativeFor reports ok=false before the migration lands.
func TestLinkNativeForNoopBeforeCutover(t *testing.T) {
	isolatedConfig(t)
	if _, ok := LinkNativeFor(t.TempDir()); ok {
		t.Error("LinkNativeFor before cutover must report ok=false")
	}
}
