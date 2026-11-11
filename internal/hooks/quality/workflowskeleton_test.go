package quality

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failAtWriter struct {
	calls  int
	failAt int
	err    error
	output bytes.Buffer
}

func (w *failAtWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return 0, w.err
	}
	return w.output.Write(p)
}

// golden commands (captured with python3 on PATH):
//   python3 workflow_skeleton.py <pattern> [--name NAME]

func readSkeletonGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "skeleton", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(b)
}

func TestRunSkeletonMatchesGolden(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"pipeline", []string{"pipeline", "--name", "review-changes"}, "pipeline.want"},
		{"parallel", []string{"parallel", "--name", "fan-out-audit"}, "parallel.want"},
		{"evaluator", []string{"evaluator", "--name", "gen-verify"}, "evaluator.want"},
		{"orchestrator", []string{"orchestrator", "--name", "plan-then-build"}, "orchestrator.want"},
		{"default name", []string{"pipeline"}, "default-name.want"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunSkeleton(tc.args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
			}
			want := readSkeletonGolden(t, tc.want)
			if stdout.String() != want {
				t.Errorf("output mismatch:\n--- got ---\n%s\n--- want ---\n%s", stdout.String(), want)
			}
		})
	}
}

func TestRunSkeletonWriteErrors(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(map[int]string{1: "pattern", 2: "note"}[failAt], func(t *testing.T) {
			sentinel := errors.New("writer exploded")
			stdout := &failAtWriter{failAt: failAt, err: sentinel}
			var stderr bytes.Buffer

			code := RunSkeleton([]string{"pipeline"}, stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1; stderr=%q", code, stderr.String())
			}
			if stdout.calls != failAt {
				t.Errorf("stdout writes = %d, want %d (no writes after first failure)", stdout.calls, failAt)
			}
			if !strings.Contains(stderr.String(), sentinel.Error()) {
				t.Errorf("stderr = %q, want writer error %q", stderr.String(), sentinel)
			}
		})
	}
}

// Each usage error exits 2 (argparse's code) with nothing on stdout and argparse's own error line last on stderr.
func TestRunSkeletonUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no pattern", nil, "the following arguments are required: pattern"},
		{"bad pattern", []string{"bogus"}, `argument pattern: invalid choice: "bogus"`},
		{"unknown flag", []string{"pipeline", "--nope"}, "unrecognized arguments: --nope"},
		{"name without value", []string{"pipeline", "--name"}, "argument --name: expected one argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := RunSkeleton(tc.args, &stdout, &stderr)
			lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
			last := lines[len(lines)-1]
			if code != 2 || stdout.Len() != 0 || !strings.HasPrefix(last, "workflow_skeleton.py: error: "+tc.want) {
				t.Fatalf("RunSkeleton(%q) = %d, stdout %q, last stderr line %q; want 2, empty, error %q",
					tc.args, code, stdout.String(), last, tc.want)
			}
		})
	}
}

func TestSkeletonEscapesWorkflowName(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := RunSkeleton([]string{"--name", "team's-flow", "pipeline"}, &out, &errb); rc != 0 {
		t.Fatalf("RunSkeleton rc=%d err=%q", rc, errb.String())
	}
	if !strings.Contains(out.String(), `name: 'team\'s-flow',`) {
		t.Errorf("skeleton meta = %q, want name: 'team\\'s-flow'", out.String())
	}
	if got := metaBlock("a\\b\nc", "d", ""); !strings.Contains(got, `name: 'a\\b\nc',`) {
		t.Errorf("metaBlock backslash/newline = %q", got)
	}
}
