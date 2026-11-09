package diff

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestGoImportersCheckoutPathWithSpaces(t *testing.T) {
	t.Setenv("GOWORK", "off")
	root := filepath.Join(t.TempDir(), "my repo")
	writeScopeFile(t, root, "go.mod", "module example.com/m\n")
	writeScopeFile(t, root, "internal/a/a.go", "package a\n")
	writeScopeFile(t, root, "internal/b/b.go", "package b\nimport _ \"example.com/m/internal/a\"\n")

	got, err := goImporters(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"internal/a": {"internal/b"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("importers in checkout with spaces = %#v, want %#v", got, want)
	}
}
