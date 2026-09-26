package diff

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// golden commands (captured with python3 on PATH):
//   python3 diff_surgeon.py --file testdata/diff/<case>.diff [--json]
// real-commit-range.diff is `git diff c6965de..0977f77` saved once (hermetic — no live git read).

func readDiffGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "diff", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

func TestRunDiffMatchesGolden(t *testing.T) {
	cases := []struct {
		name string
		diff string
		want string
	}{
		{"synthetic text", "synthetic-noise.diff", "synthetic-noise.want"},
		{"synthetic json", "synthetic-noise.diff", "synthetic-noise.want.json"},
		{"real-commit text", "real-commit-range.diff", "real-commit-range.want"},
		{"real-commit json", "real-commit-range.diff", "real-commit-range.want.json"},
		{"empty text", "empty.diff", "empty.want"},
		{"empty json", "empty.diff", "empty.want.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jsonOut := filepath.Ext(tc.want) == ".json"
			args := []string{"--file", filepath.Join("testdata", "diff", tc.diff)}
			if jsonOut {
				args = append(args, "--json")
			}
			var stdout, stderr bytes.Buffer
			code := RunDiff(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			want := readDiffGolden(t, tc.want)
			if stdout.String() != want {
				t.Errorf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", stdout.String(), want)
			}
		})
	}
}

// Findings are advisory (exit 0); usage errors are not: a missing --file exits 1 with one clean line.
func TestRunDiffMissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunDiff([]string{"--file", "nosuch.diff"}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "nosuch.diff not found") {
		t.Fatalf("exit %d stderr %q, want 1 and a not-found line", code, stderr.String())
	}
}

func TestRunDiffBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunDiff([]string{"--nope"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (argparse unrecognized arguments)", code)
	}
}
