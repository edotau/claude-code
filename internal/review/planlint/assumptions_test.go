package planlint

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// golden commands (captured with python3 on PATH):
//   cd testdata/assumptions && python3 assumption_linter.py <file> [--json]
//   echo "..." | python3 assumption_linter.py - [--json]        (stdin-case.*)

func readAssumptionsGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "assumptions", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

func TestRunAssumptionsMatchesGolden(t *testing.T) {
	// Goldens were captured with `cd testdata/assumptions && python3 assumption_linter.py <file>`,
	// so "source" in the output is the bare filename — chdir here to match that relative path.
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"plan text", []string{"plan-standalone-harness.md"}, "plan-standalone-harness.md.want"},
		{"plan json", []string{"plan-standalone-harness.md", "--json"}, "plan-standalone-harness.md.want.json"},
		{"skill text", []string{"simplicity-skill.md"}, "simplicity-skill.md.want"},
		{"skill json", []string{"simplicity-skill.md", "--json"}, "simplicity-skill.md.want.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := readAssumptionsGolden(t, tc.want)
			t.Chdir(filepath.Join("testdata", "assumptions"))
			var stdout, stderr bytes.Buffer
			code := RunAssumptions(tc.args, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			if stdout.String() != want {
				t.Errorf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", stdout.String(), want)
			}
		})
	}
}

func TestRunAssumptionsStdin(t *testing.T) {
	in := readAssumptionsGolden(t, "stdin-case.txt")
	wantText := readAssumptionsGolden(t, "stdin-case.want")
	wantJSON := readAssumptionsGolden(t, "stdin-case.want.json")

	var stdout, stderr bytes.Buffer
	if code := RunAssumptions([]string{"-"}, strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != wantText {
		t.Errorf("text mismatch:\ngot:  %q\nwant: %q", stdout.String(), wantText)
	}

	stdout.Reset()
	if code := RunAssumptions([]string{"-", "--json"}, strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != wantJSON {
		t.Errorf("json mismatch:\ngot:  %s\nwant: %s", stdout.String(), wantJSON)
	}
}

func TestRunAssumptionsMissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunAssumptions([]string{"nosuch.md"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (a gate must not pass on a missing plan file)", code)
	}
	if stderr.String() != "[error] nosuch.md not found\n" {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunAssumptionsBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunAssumptions([]string{"--nope"}, strings.NewReader(""), &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d for a bad flag, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty on a bad flag", stdout.String())
	}
}

// -h after a positional input prints the usage page to stdout, exit 0, stderr empty.
func TestRunAssumptionsHelpAfterPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunAssumptions([]string{"somefile.md", "-h"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty on -h", stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "usage: claude-code review assumptions [<input>] [flags]\n") {
		t.Errorf("want the review assumptions usage page:\n%s", stdout.String())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// A failed stdout write must exit 1 with a stderr line, in both text and JSON modes.
func TestRunAssumptionsStdoutWriteError(t *testing.T) {
	for _, args := range [][]string{{"-"}, {"-", "--json"}} {
		var stderr bytes.Buffer
		code := RunAssumptions(args, strings.NewReader("We should probably add caching."), failWriter{}, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "writing output") {
			t.Errorf("args %v: code=%d stderr=%q; want 1 and 'writing output'", args, code, stderr.String())
		}
	}
}

func TestContinuationLineVerificationCounts(t *testing.T) {
	missing := func(text string) int {
		n := 0
		for _, f := range lintText(text) {
			if f.Category == "missing-verification" {
				n++
			}
		}
		return n
	}
	if got := missing("1. Create the endpoint.\n   verify: check HTTP status 200.\n"); got != 0 {
		t.Errorf("indented verify line: %d missing-verification findings, want 0", got)
	}
	if got := missing("1. Create the endpoint.\n   Then wire it.\n"); got != 1 {
		t.Errorf("no verification: %d missing-verification findings, want 1", got)
	}
}
