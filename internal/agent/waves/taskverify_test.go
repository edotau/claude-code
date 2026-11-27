package waves

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/inspect"
	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/dev/worktree"
	"github.com/edotau/claude-code/internal/platform/hosttest"
)

func TestVerifyTaskDeduplicatesGoChecks(t *testing.T) {
	dir := t.TempDir()
	hosttest.WriteRel(t, dir, "go.mod", "module x\n")
	hosttest.WriteRel(t, dir, "pkg/p.go", "package pkg\n")
	old := taskCheckRun
	t.Cleanup(func() { taskCheckRun = old })
	var steps []string
	taskCheckRun = func(_ context.Context, _ TaskVerifyInput, id, step, cmd string) TaskCheck {
		steps = append(steps, step)
		return TaskCheck{Task: id, Cmd: cmd}
	}
	in := TaskVerifyInput{Dir: dir, Files: []string{"pkg/p.go"}, Task: plans.Task{ID: "1.5",
		Title: "batch verify: go build ./... && go vet ./pkg && go test ./pkg"}}
	checks := VerifyTask(context.Background(), in)
	if len(checks) != 1 || !slices.Equal(steps, []string{"diff"}) {
		t.Fatalf("duplicated default gates: %+v steps %v", checks, steps)
	}
	steps = nil
	in.Task.Title = "batch verify: go test ./pkg/..."
	if checks = VerifyTask(context.Background(), in); len(checks) != 1 || !slices.Equal(steps, []string{"diff"}) {
		t.Fatalf("covered recursive package repeated gates: %+v steps %v", checks, steps)
	}
	steps = nil
	in.Task.Title = "batch verify: claude-code doctor"
	checks = VerifyTask(context.Background(), in)
	if len(checks) != 2 || !slices.Equal(steps, []string{"verify", "diff"}) {
		t.Fatalf("custom CLI verify was dropped: %+v steps %v", checks, steps)
	}
}

func TestVerifyTaskRedundantSubset(t *testing.T) {
	files := []string{"pkg/p.go"}
	for _, cmd := range []string{"go test ./pkg", "go test -run '^TestP$' ./pkg", "rtk go build ./... && go vet ./pkg"} {
		if !redundantGoVerify("", cmd, files) {
			t.Errorf("redundant command not detected: %s", cmd)
		}
	}
	for _, cmd := range []string{"go test ./other", "go test ./...", "go test -race ./pkg", "GOOS=linux go build ./pkg", "claude-code doctor"} {
		if redundantGoVerify("", cmd, files) {
			t.Errorf("custom or uncovered command dropped: %s", cmd)
		}
	}
}

func taskStampFixture(t *testing.T) (string, string, []plans.Task) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := hosttest.Repo(t)
	hosttest.WriteRel(t, dir, "go.mod", "module x\n")
	hosttest.WriteRel(t, dir, "pkg/p.go", "package pkg\nconst N = 1\n")
	hosttest.In(t, dir, "add", "-A")
	hosttest.In(t, dir, "commit", "-qm", "module")
	hosttest.WriteRel(t, dir, "pkg/p.go", "package pkg\nconst N = 2\n")
	key, err := inspect.ComputeDiffStampKey(context.Background(), dir, "HEAD", []string{"pkg"}, []string{"pkg/p.go"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inspect.WriteStamp(key, inspect.StampMeta{Passed: true, Gates: inspect.DefaultStampGates}); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, dir, "add", "-A")
	hosttest.In(t, dir, "commit", "-qm", "change")
	sha := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "HEAD"))
	return dir, sha, []plans.Task{{ID: "1.5", Title: "batch verify: go test ./pkg"}}
}

func TestVerifyTaskCommitStampReuseBeforeAcquire(t *testing.T) {
	dir, sha, tasks := taskStampFixture(t)
	old := acquireVerifySlot
	t.Cleanup(func() { acquireVerifySlot = old })
	acquired := false
	acquireVerifySlot = func(context.Context, string, string, string) (*worktree.Slot, error) {
		acquired = true
		return nil, errors.New("unexpected AcquireSlot")
	}
	hosttest.WriteRel(t, dir, "pkg/p.go", "package pkg\nconst N = 3\n")
	out, err := VerifyCommit(context.Background(), dir, sha, tasks)
	if err != nil || acquired || len(out["1.5"]) != 1 || out["1.5"][0].Cmd != "stamp-reused" || !AllOK(out["1.5"]) {
		t.Fatalf("stamp reuse: %+v err %v acquired %v", out, err, acquired)
	}
	if out["1.5"][0].Took != 0 {
		t.Fatal("synthetic stamp check must take zero time")
	}
	tasks[0].Title = "custom verify: claude-code doctor"
	if _, ok := reuseCommitStamp(context.Background(), dir, sha, tasks, []string{"pkg/p.go"}); ok {
		t.Fatal("stamp suppressed custom CLI verify")
	}
}

func TestVerifyTaskWaveDefaultsRequireUnionPass(t *testing.T) {
	r := &runner{h: runHooks{check: CheckEnv{Diff: func(inspect.DiffOptions) int {
		t.Fatal("post-union per-leaf default diff called")
		return 1
	}}}}
	w := WaveState{Gates: []inspect.GateResult{{Name: "test", Status: "ok"}}}
	for _, cmd := range []string{"", DefaultVerify([]string{"pkg/p.go"}), "claude-code agents diff --check"} {
		ok, result, err := r.checkedLeaf(context.Background(), w, Spec{ID: "1", Owns: []string{"pkg/p.go"}, Verify: cmd})
		if !ok || result != nil || err != nil {
			t.Fatalf("default leaf check: %v %+v %v", ok, result, err)
		}
	}
}

func TestVerifyTaskReleaseRunsDefault(t *testing.T) {
	calls := 0
	r := &runner{dir: t.TempDir(), h: runHooks{check: CheckEnv{Exec: func(context.Context, string, string, []string, io.Writer) (int, error) {
		calls++
		return 1, nil
	}}}}
	ready := []Spec{{ID: "1", Owns: []string{"pkg/p.go"}}}
	waits := map[string]wait{"2": {after: []string{"1"}}}
	passed, err := r.released(context.Background(), ready, Envelope{}, "1", waits)
	if err != nil || passed || calls != 1 {
		t.Fatalf("released failing prerequisite: pass %v calls %d err %v", passed, calls, err)
	}
}

func TestVerifyTaskDeferredChecks(t *testing.T) {
	dir := t.TempDir()
	hosttest.WriteRel(t, dir, "go.mod", "module x\n")
	hosttest.WriteRel(t, dir, "pkg/p.go", "package pkg\n")
	for _, cmd := range []string{"go test -race ./pkg", "claude-code doctor"} {
		checks := DeferredChecks(dir, plans.Task{Title: "task verify: " + cmd}, []string{"pkg/p.go"})
		if len(checks) != 1 || checks[0].Command != cmd || checks[0].Generated {
			t.Fatalf("lost custom requirement: %+v", checks)
		}
	}
	checks := DeferredChecks(dir, plans.Task{Title: "task verify: go test ./pkg"}, []string{"pkg/p.go"})
	if len(checks) != 0 {
		t.Fatalf("redundant default retained: %+v", checks)
	}
}

func TestVerifyTaskDeferredPython(t *testing.T) {
	dir := t.TempDir()
	hosttest.WriteRel(t, dir, "pyproject.toml", "[project]\nname = 'sample'\n")
	hosttest.WriteRel(t, dir, "tests/test_p.py", "def test_p(): pass\n")
	checks := DeferredChecks(dir, plans.Task{}, []string{"tests/test_p.py"})
	if len(checks) != 1 || !checks[0].Generated || !strings.Contains(checks[0].Command, "pytest") {
		t.Fatalf("lost Python gate: %+v", checks)
	}
}

func TestVerifyTaskRejectsPartialDiffProof(t *testing.T) {
	dir, _, tasks := taskStampFixture(t)
	hosttest.WriteRel(t, dir, "pkg/unchecked.go", "package pkg\n")
	key, err := inspect.ComputeDiffStampKey(context.Background(), dir, "HEAD", []string{"pkg"}, []string{"pkg/p.go"})
	if err != nil {
		t.Fatal(err)
	}
	if err := inspect.WriteStamp(key, inspect.StampMeta{Passed: true, Gates: inspect.DefaultStampGates}); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, dir, "add", "-A")
	hosttest.In(t, dir, "commit", "-qm", "unchecked file")
	sha := strings.TrimSpace(hosttest.In(t, dir, "rev-parse", "HEAD"))
	if _, ok := reuseCommitStamp(context.Background(), dir, sha, tasks, []string{"pkg/unchecked.go"}); ok {
		t.Fatal("partial file proof waived a different commit diff")
	}
}

func TestVerifyTaskLines(t *testing.T) {
	dir := t.TempDir()
	in := TaskVerifyInput{Dir: dir, OutDir: dir}
	pass := runCheck(context.Background(), in, "1.1", "verify", "true").Line()
	fail := runCheck(context.Background(), in, "1.2", "verify", "false").Line()
	if !strings.HasPrefix(pass, "verify 1.1: ") || !strings.Contains(pass, " ok (") {
		t.Errorf("pass line = %q", pass)
	}
	if !strings.HasPrefix(fail, "verify 1.2: ") || !strings.Contains(fail, "FAIL exit 1") {
		t.Errorf("fail line = %q", fail)
	}
}
