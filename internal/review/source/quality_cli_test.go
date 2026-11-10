package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A traversal error inside an excluded directory (node_modules) is neither analyzed nor reported as a failure.
func TestRunQualityIgnoresExcludedDirectoryTraversalError(t *testing.T) {
	root := t.TempDir()
	writeFx(t, root, "ok.py", "value = 1\n")
	locked := filepath.Join(root, "node_modules")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	unreadableDir(t, locked)
	var stdout, stderr bytes.Buffer
	if code := RunQuality([]string{root, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	var report directoryReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.FilesAnalyzed != 1 || len(report.FilesFailed) != 0 {
		t.Fatalf("analyzed = %d, failed = %v; want one analyzed file and no failures", report.FilesAnalyzed, report.FilesFailed)
	}
}

// unreadableDir nests directories under parent past PATH_MAX (1024 darwin, 4096 linux), so ReadDir fails for any user.
func unreadableDir(t *testing.T, parent string) string {
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
	return path
}

func TestEmitQualityJSONReplacesWithoutTruncating(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "report.json")
	writeFx(t, dir, "report.json", "previous report")
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
