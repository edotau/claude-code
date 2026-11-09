package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestQualityPythonMultilineClassHeaderGodClass(t *testing.T) {
	var src strings.Builder
	src.WriteString("class Service(\n    Base,\n):\n")
	for i := range 21 {
		fmt.Fprintf(&src, "    def method%d(self):\n        pass\n", i)
	}
	src.WriteString("\ndef standalone():\n    pass\n")
	report, err := QualityContent("service.py", src.String(), "python")
	if err != nil {
		t.Fatal(err)
	}
	want := []Smell{{
		Type: "god_class", Severity: "high", Location: "Service",
		Message: "Class 'Service' has 21 methods (max: 20)",
	}}
	if !slices.Equal(report.Smells, want) {
		t.Fatalf("smells = %+v, want %+v for a multiline class header", report.Smells, want)
	}
}

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
