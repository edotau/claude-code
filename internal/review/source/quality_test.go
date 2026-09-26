package source

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// golden commands (captured against this exact testdata dir, python3 on PATH):
//   cd testdata/quality && python3 code_quality_checker.py <path> [--json]
// See testdata/quality/README for the full list.

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "quality", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

func TestRunQualityMatchesGolden(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"file text", []string{"paths.go"}, "paths.go.want"},
		{"file json", []string{"paths.go", "--json"}, "paths.go.want.json"},
		{"js file text", []string{"gate-loop.js"}, "gate-loop.js.want"},
		{"js file json", []string{"gate-loop.js", "--json"}, "gate-loop.js.want.json"},
		{"dir text", []string{"."}, "dir.want"},
		{"dir json", []string{".", "--json"}, "dir.want.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(filepath.Join("testdata", "quality"))
			var stdout, stderr bytes.Buffer
			code := RunQuality(tc.args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			want, err := os.ReadFile(tc.want)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			// Python's print() adds a trailing newline; the golden capture already has it.
			if stdout.String() != string(want) {
				t.Errorf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", stdout.String(), string(want))
			}
		})
	}
}

func TestRunQualityMissingPath(t *testing.T) {
	want := readGolden(t, "notfound.want")
	t.Chdir(filepath.Join("testdata", "quality"))
	var stdout, stderr bytes.Buffer
	code := RunQuality([]string{"nonexistent.go"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stderr.String() != want {
		t.Errorf("stderr mismatch:\ngot:  %q\nwant: %q", stderr.String(), want)
	}
}

func TestRunQualityBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunQuality([]string{"--nope", "x.go"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit code = 0 for a bad flag, want nonzero")
	}
}
