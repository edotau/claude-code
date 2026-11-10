package diff

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGoImportersSymlinkedRoot(t *testing.T) {
	t.Setenv("GOWORK", "off")
	real := filepath.Join(t.TempDir(), "real")
	writeScopeFile(t, real, "go.mod", "module example.com/m\n")
	writeScopeFile(t, real, "internal/a/a.go", "package a\n")
	writeScopeFile(t, real, "internal/b/b.go", "package b\nimport _ \"example.com/m/internal/a\"\n")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	got, err := goImporters(link)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"internal/a": {"internal/b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("importers via symlinked root = %#v, want %#v", got, want)
	}
}
