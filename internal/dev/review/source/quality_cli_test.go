package source

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unreadableDir nests directories under parent past PATH_MAX (1024 darwin, 4096 linux), so ReadDir fails for any user.
func unreadableDir(t *testing.T, parent string) {
	t.Helper()
	r, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	seg, path := strings.Repeat("d", 200), parent
	for len(path) <= 4200 {
		err := r.Mkdir(seg, 0o755)
		next, oerr := r.OpenRoot(seg)
		_ = r.Close()
		if err = errors.Join(err, oerr); err != nil {
			t.Fatal(err)
		}
		r, path = next, filepath.Join(path, seg)
	}
	_ = r.Close()
	if _, err := os.ReadDir(path); err == nil {
		t.Fatalf("ReadDir of a %d-byte path succeeded; the fixture needs a path the OS rejects", len(path))
	}
}

// Pins Python json.dumps(indent=2) bytes: ensure_ascii (incl. surrogate pairs), no HTML escaping.
func TestPyMarshalIndentEnsureASCII(t *testing.T) {
	got, err := PyMarshalIndent(map[string]string{"k": "é<>&😀"})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"k\": \"\\u00e9<>&\\ud83d\\ude00\"\n}"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
