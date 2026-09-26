package plan

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// golden commands (captured with python3 on PATH):
//   cd testdata/goals && python3 goal_verifier.py <file> [--json]
//   echo "..." | python3 goal_verifier.py - [--json]        (stdin-case.*)

func readGoalsGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "goals", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

func TestRunGoalsMatchesGolden(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"plan text", []string{"plan-standalone-harness.md"}, "plan-standalone-harness.md.want"},
		{"plan json", []string{"plan-standalone-harness.md", "--json"}, "plan-standalone-harness.md.want.json"},
		{"noplan json", []string{"noplan.txt", "--json"}, "noplan.want.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := readGoalsGolden(t, tc.want)
			t.Chdir(filepath.Join("testdata", "goals"))
			var stdout, stderr bytes.Buffer
			code := RunGoals(tc.args, strings.NewReader(""), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			if stdout.String() != want {
				t.Errorf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", stdout.String(), want)
			}
		})
	}
}

func TestRunGoalsStdin(t *testing.T) {
	in := readGoalsGolden(t, "stdin-case.txt")
	wantText := readGoalsGolden(t, "stdin-case.want")
	wantJSON := readGoalsGolden(t, "stdin-case.want.json")

	var stdout, stderr bytes.Buffer
	if code := RunGoals([]string{"-"}, strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != wantText {
		t.Errorf("text mismatch:\ngot:  %q\nwant: %q", stdout.String(), wantText)
	}

	stdout.Reset()
	if code := RunGoals([]string{"-", "--json"}, strings.NewReader(in), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if stdout.String() != wantJSON {
		t.Errorf("json mismatch:\ngot:  %s\nwant: %s", stdout.String(), wantJSON)
	}
}

func TestRunGoalsMissingFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunGoals([]string{"nosuch.md"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (matches Python's sys.exit(0) on a missing file)", code)
	}
	if stderr.String() != "[error] nosuch.md not found\n" {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestRunGoalsBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunGoals([]string{"--nope"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit code = 0 for a bad flag, want nonzero")
	}
}
