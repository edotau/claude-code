package source

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRunQualityIgnoresExcludedDirectoryTraversalError(t *testing.T) {
	root := qualityRootWithUnreadableExcludedDirectory(t)
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

func qualityRootWithUnreadableExcludedDirectory(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFx(t, root, "ok.py", "value = 1\n")
	locked := filepath.Join(root, "node_modules")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o755); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("requires a directory that the current user cannot read")
	}
	return root
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
