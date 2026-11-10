package memory

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBankFilesReturnsIndependentSlice(t *testing.T) {
	want := append([]string(nil), bankFiles...)
	got := BankFiles()
	original := got[0]
	t.Cleanup(func() { got[0] = original })
	got[0] = "caller-owned.md"
	if next := BankFiles(); !reflect.DeepEqual(next, want) {
		t.Fatalf("caller mutation changed BankFiles: got %v, want %v", next, want)
	}
}

func TestInitLeavesNoPartialFileWhenWriteFails(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		if err := os.Remove(filepath.Join(dir, en.Name())); err != nil {
			t.Fatal(err)
		}
	}
	orig := writeBody
	writeBody = func(f *os.File, body string) error { // half the body lands, then the disk fills
		_, _ = f.WriteString(body[:len(body)/2])
		return errors.New("no space left on device")
	}
	t.Cleanup(func() { writeBody = orig })
	if _, err = Init(root); err == nil {
		t.Fatal("Init succeeded although every write failed")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("failed Init left %d file(s) behind, first %q", len(left), left[0].Name())
	}
}

// Init seeds every missing template byte-exact, keeps a file that already exists and leaves no temp file.
func TestInitLeavesNoTempFile(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	const own = "# my own conventions\n"
	write(t, filepath.Join(Dir(root), "conventions.md"), own)
	dir, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range templates() {
		if name == "conventions.md" {
			want = own
		}
		if got := mustRead(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*")); len(m) != 0 {
		t.Errorf("Init left temp files behind: %v", m)
	}
}
