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
		{"template text", []string{"plan-template.md"}, "plan-template.md.want"},
		{"template json", []string{"plan-template.md", "--json"}, "plan-template.md.want.json"},
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
	if code != 2 {
		t.Fatalf("exit code = %d for a bad flag, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty on a bad flag", stdout.String())
	}
}

// -h after a positional input prints the usage page to stdout, exit 0, stderr empty.
func TestRunGoalsHelpAfterPositional(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunGoals([]string{"somefile.md", "-h"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.HasPrefix(stdout.String(), "usage: claude-code review goals [<input>] [flags]\n") {
		t.Errorf("stdout = %q, want the review goals usage page", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty on -h", stderr.String())
	}
}

func TestRunGoalsFlagsOversizedTasks(t *testing.T) {
	text := readGoalsGolden(t, "oversized-tasks.md")
	tasks := findOversizedTasks(text)
	if len(tasks) != 3 {
		var titles []string
		for _, ot := range tasks {
			titles = append(titles, ot.Title+" :: "+ot.Reason)
		}
		t.Fatalf("oversized tasks = %d, want 3; got: %v", len(tasks), titles)
	}
	if !strings.Contains(tasks[0].Reason, "packages") {
		t.Errorf("task 0 reason = %q, want a package-count reason", tasks[0].Reason)
	}
	if !strings.Contains(tasks[1].Reason, "findings") {
		t.Errorf("task 1 reason = %q, want a findings-count reason", tasks[1].Reason)
	}
	if !strings.Contains(tasks[2].Reason, "owns:") {
		t.Errorf("task 2 reason = %q, want a missing-owns reason", tasks[2].Reason)
	}

	var stdout, stderr bytes.Buffer
	if code := RunGoals([]string{filepath.Join("testdata", "goals", "oversized-tasks.md"), "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"oversized_tasks"`) {
		t.Errorf("json output missing oversized_tasks: %s", stdout.String())
	}

	stdout.Reset()
	if code := RunGoals([]string{filepath.Join("testdata", "goals", "oversized-tasks.md")}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Oversized tasks:") {
		t.Errorf("text output missing Oversized tasks section: %s", stdout.String())
	}
}

func TestRunGoalsLeafSizedTasksClean(t *testing.T) {
	text := readGoalsGolden(t, "leaf-sized-tasks.md")
	if tasks := findOversizedTasks(text); len(tasks) != 0 {
		t.Fatalf("leaf-sized fixture flagged as oversized: %v", tasks)
	}
}

func TestFindOversizedTasksBoundaries(t *testing.T) {
	got := map[string]string{}
	for _, ot := range findOversizedTasks(readGoalsGolden(t, "oversized-boundaries.md")) {
		got[strings.Fields(ot.Title)[0]] = ot.Reason
	}
	cases := []struct {
		id, want string // want "" = not flagged
	}{
		{"A1", ""},                   // nested segments (sub/, deep/, x/) are not packages
		{"A2", "names 4 packages"},   // backtick, `(`, whitespace and `,` all start a path token
		{"A3", "names 4 packages"},   // ./internal/<x> in a verify command still counts
		{"A4", ""},                   // (codex/copilot) is a runner alternative, not a package
		{"B1", "names 4 packages"},   // cmd/cli and internal/cli are distinct
		{"C1", "no `owns:` segment"}, // owns: at end of line is empty
		{"C2", "no `owns:` segment"}, // owns: followed by an em dash is empty
		{"C3", ""},                   // a filled owns: is clean
		{"D1", ""},                   // inside a ``` fence
		{"D2", ""},                   // inside a ~~~ fence
		{"D3", ""},                   // ``` inside ~~~ does not close it
		{"D4", "no `owns:` segment"}, // fences closed, checklist lines count again
	}
	for _, tc := range cases {
		reason, flagged := got[tc.id]
		switch {
		case tc.want == "" && flagged:
			t.Errorf("%s flagged (%q), want clean", tc.id, reason)
		case tc.want != "" && !strings.Contains(reason, tc.want):
			t.Errorf("%s reason = %q, want it to contain %q", tc.id, reason, tc.want)
		}
	}
	if len(got) != 6 {
		t.Errorf("flagged %d tasks, want 6: %v", len(got), got)
	}
}

func TestExtractStepsPhaseBoundaries(t *testing.T) {
	text := "## Phase 1 — Parse\n**Verify:** go test ./internal/paths\n### Details\n1. Read input\n### Phase 2 — Render\n**Verify:** should work\n## Tasks\n- [ ] go test ./...\n## Risks\ngrep final\n## Context\ndiff final\n"
	steps := extractSteps(text)
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
	if !strings.Contains(steps[0], "1. Read input") {
		t.Error("phase lost nested section")
	}
	if score, _ := scoreStep(steps[1]); score != 1 {
		t.Errorf("vague phase score = %d, want 1", score)
	}
	if finalRe().MatchString(steps[1]) {
		t.Error("non-phase sections leaked into final step")
	}
}
