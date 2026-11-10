package memory

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRecallLoadTruncatesOnRuneBoundary(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id, err := Remember(root, "feedback", strings.Repeat("—", recallLoadCap), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	recallLoad(&out, &errw, []SearchHit{{ID: id}}, 1)
	if out.Len() == 0 || !utf8.Valid(out.Bytes()) {
		t.Errorf("recallLoad output empty or invalid UTF-8 (err %q)", errw.String())
	}
}

func TestSplitOverflowSurfacesReadErrorNotAbsence(t *testing.T) {
	dir := t.TempDir()
	if _, _, _, err := splitOverflow(filepath.Join(dir, "absent.md"), 5); err != nil {
		t.Errorf("absent file: err = %v, want nil", err)
	}
	bad := filepath.Join(dir, "decisionLog.md")
	if err := os.Mkdir(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveFile(bad, 5, time.Now()); err == nil {
		t.Error("archiveFile on an unreadable file returned nil, want the read error")
	}
}

func TestExtractBlockSurfacesReadError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "decisionLog.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := extractBlock(dir, "abcd1234")
	if err == nil || errors.Is(err, errNoBlock) {
		t.Errorf("extractBlock err = %v, want the read error, not errNoBlock", err)
	}
}

func TestReadIfExists(t *testing.T) {
	dir := t.TempDir()
	if b, err := readIfExists(filepath.Join(dir, "nope")); b != nil || err != nil {
		t.Errorf("absent: %q, %v; want empty, nil", b, err)
	}
	if _, err := readIfExists(dir); err == nil {
		t.Error("directory read returned nil error")
	}
}

func TestDigestInputBoundsBeforeReading(t *testing.T) {
	dir := t.TempDir()
	arch := archiveDir(dir)
	if err := os.MkdirAll(arch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(arch, "a.md"), bytes.Repeat([]byte("x"), digestMaxInput+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := digestInput(dir); err == nil || !strings.Contains(err.Error(), "needs-scope") {
		t.Errorf("err = %v, want needs-scope", err)
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

func TestInitLeavesNoTempFile(t *testing.T) {
	tempConfig(t)
	dir, err := Init(repoRoot(t, "proj"))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*")); len(m) != 0 {
		t.Errorf("Init left temp files behind: %v", m)
	}
}
