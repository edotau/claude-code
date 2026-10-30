package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStatHelpers(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(file, []byte("  hello \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope")

	if !IsFile(file) || IsFile(dir) || IsFile(missing) {
		t.Errorf("IsFile: file=%v dir=%v missing=%v", IsFile(file), IsFile(dir), IsFile(missing))
	}
	if !IsDir(dir) || IsDir(file) || IsDir(missing) {
		t.Errorf("IsDir: dir=%v file=%v missing=%v", IsDir(dir), IsDir(file), IsDir(missing))
	}
	if !FileExists(file) || !FileExists(dir) || FileExists(missing) {
		t.Errorf("FileExists: file=%v dir=%v missing=%v", FileExists(file), FileExists(dir), FileExists(missing))
	}
}

func TestDirSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{"a": "123", "sub/b": "45678"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := DirSize(dir); got != 8 {
		t.Errorf("DirSize(dir) = %d, want 8", got)
	}
	if got := DirSize(filepath.Join(dir, "a")); got != 3 {
		t.Errorf("DirSize(file) = %d, want 3", got)
	}
	if got := DirSize(filepath.Join(dir, "nope")); got != 0 {
		t.Errorf("DirSize(missing) = %d, want 0", got)
	}
}

func TestSafeName(t *testing.T) {
	for name, ok := range map[string]bool{
		"claude":            true,
		"databricks-expert": true,
		"../../escape":      false,
		"a/b":               false,
		`a\b`:               false,
		".":                 false,
		"..":                false,
		"":                  false,
		".hidden":           false,
		"/etc/passwd":       false,
	} {
		if got := SafeName(name); got != ok {
			t.Errorf("SafeName(%q) = %v, want %v", name, got, ok)
		}
	}
}
