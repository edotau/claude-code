package memory

import (
	"errors"
	"io/fs"
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
	own := BankFor(root).Prefix() + "*"
	entries, _ := filepath.Glob(filepath.Join(dir, own))
	for _, en := range entries {
		if err := os.Remove(en); err != nil {
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
	if left, _ := filepath.Glob(filepath.Join(dir, own)); len(left) != 0 {
		t.Errorf("failed Init left %d file(s) behind, first %q", len(left), left[0])
	}
}

// Init seeds every missing template byte-exact, keeps a file that already exists and leaves no temp file.
func TestInitLeavesNoTempFile(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	const own = "# my own conventions\n"
	write(t, BankFor(root).Path("conventions.md"), own)
	dir, err := Init(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range templates() {
		if name == "conventions.md" {
			want = own
		}
		if got := mustRead(t, BankFor(root).Path(name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*")); len(m) != 0 {
		t.Errorf("Init left temp files behind: %v", m)
	}
}

// createExcl's write failing midway (disk full, file-size limit) removes the partial file, so a retry lands the body.
func TestPersistenceCreateExclRemovesPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projectContext.md")
	orig := writeBody
	t.Cleanup(func() { writeBody = orig })
	writeBody = func(f *os.File, body string) error {
		_, _ = f.WriteString(body[:len(body)/2])
		return errors.New("file too large")
	}
	if err := createExcl(path, "partial scaffold\n"); err == nil {
		t.Fatal("expected a partial-write error")
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed fallback left a partial file: %v", err)
	}
	writeBody = orig
	if err := createExcl(path, "retry\n"); err != nil {
		t.Fatal(err)
	}
	if got := aliasRead(t, path); got != "retry\n" {
		t.Errorf("retry retained partial scaffold: %q", got)
	}
}
