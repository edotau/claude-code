package source

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
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

func TestEmitQualityJSONReplacesWithoutTruncating(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "report.json")
	hosttest.WriteRel(t, dir, "report.json", "previous report")
	old, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var stdout bytes.Buffer
	if err := emitQualityJSON(map[string]int{"files": 1}, output, &stdout); err != nil {
		t.Fatal(err)
	}
	previous, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	if string(previous) != "previous report" {
		t.Errorf("open previous file = %q, want unchanged contents", previous)
	}
	current, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "{\n  \"files\": 1\n}" {
		t.Errorf("replacement = %q, want complete JSON report", current)
	}
	if stdout.String() != "Results written to "+output+"\n" {
		t.Errorf("stdout = %q, want success message naming output", stdout.String())
	}
}
