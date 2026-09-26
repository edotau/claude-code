package source

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// golden commands (captured against this exact testdata dir, python3 on PATH):
//   cd testdata/complexity && python3 complexity_checker.py <args> [--json]
// .py complexity findings are a Go ESTIMATE (not full AST parity) — only shape/structural
// fields are asserted for python; .js/.ts findings are regex-based and must match byte-for-byte.

func readComplexityGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "complexity", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

func TestRunComplexityJSMatchesGoldenExactly(t *testing.T) {
	t.Chdir(filepath.Join("testdata", "complexity"))
	want := readComplexityGoldenAbs(t, "gate-loop.js.want")
	wantJSON := readComplexityGoldenAbs(t, "gate-loop.js.want.json")

	var stdout, stderr bytes.Buffer
	if code := RunComplexity([]string{"gate-loop.js"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != want {
		t.Errorf("text mismatch:\ngot:  %q\nwant: %q", stdout.String(), want)
	}

	stdout.Reset()
	if code := RunComplexity([]string{"gate-loop.js", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != wantJSON {
		t.Errorf("json mismatch:\ngot:  %s\nwant: %s", stdout.String(), wantJSON)
	}
}

// readComplexityGoldenAbs reads relative to the ORIGINAL test dir, safe to call after t.Chdir.
func readComplexityGoldenAbs(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(name)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

// TestRunComplexityPythonShapeOnly checks structure, not exact scores/findings (estimate, not AST parity).
func TestRunComplexityPythonShapeOnly(t *testing.T) {
	t.Chdir(filepath.Join("testdata", "complexity"))
	var stdout, stderr bytes.Buffer
	if code := RunComplexity([]string{"assumption_linter.py", "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, stdout.String())
	}
	for _, key := range []string{"status", "threshold", "files_analyzed", "total_findings", "average_score", "verdict", "results"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q in output: %s", key, stdout.String())
		}
	}
	results, ok := got["results"].([]interface{})
	if !ok || len(results) != 1 {
		t.Fatalf("expected 1 result, got %v", got["results"])
	}
	r0 := results[0].(map[string]interface{})
	if r0["file"] != "assumption_linter.py" || r0["language"] != "python" {
		t.Errorf("result[0] shape wrong: %v", r0)
	}
}

func TestRunComplexityNoMatchingFiles(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunComplexity([]string{"nosuchfile.py", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (advisory, always exits 0)", code)
	}
	var got map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\noutput: %s", err, stdout.String())
	}
	if got["status"] != "error" {
		t.Errorf("status = %q, want error", got["status"])
	}
}

func TestRunComplexityBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunComplexity([]string{"--nope", "x.py"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit code = 0 for a bad flag, want nonzero")
	}
}
