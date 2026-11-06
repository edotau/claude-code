package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonPathResolvesThroughMissingNestedDirs(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got := CanonPath(filepath.Join(link, "a", "b", "c.txt"))
	realDir, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(realDir, "a", "b", "c.txt"); got != want {
		t.Fatalf("CanonPath = %q, want %q", got, want)
	}
}
