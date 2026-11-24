package autofix

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	repairSHA      = "abcdef0123456789"
	redGateFixture = "ok gofmt (0.1s)\nFAIL test (1.2s)\n--- FAIL: TestX at /Users/x/y.go\n" +
		"FAIL lint (0.3s)\nsample.go:3: golangci-lint/errcheck: unchecked\n"
)

type repairCall struct {
	dir, name string
	args      []string
}

type repairFake struct {
	lane         string
	changed, red bool
	fail         string // joined args of the one call that errors
}

func fakeRepair(t *testing.T, changed, red bool) (string, string, *[]repairCall) {
	return fakeRepairFailing(t, changed, red, "")
}

func fakeRepairFailing(t *testing.T, changed, red bool, fail string) (string, string, *[]repairCall) {
	t.Helper()
	top, lane := t.TempDir(), t.TempDir()
	for _, file := range []string{".git", "sample.go", "guide.md", "internal/schemas/testdata/writers/example.txt"} {
		path := filepath.Join(lane, file)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("z\tlast\na\tfirst\na\tfirst\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldRun, oldBin, oldCap := run, harnessBin, laneCap
	t.Cleanup(func() { run, harnessBin, laneCap = oldRun, oldBin, oldCap })
	harnessBin = func() (string, error) { return "harness", nil }
	laneCap = func(context.Context, string) error { return nil }
	var calls []repairCall
	fake := repairFake{lane, changed, red, fail}
	run = func(_ context.Context, dir, name string, args ...string) (string, error) {
		calls = append(calls, repairCall{dir, name, slices.Clone(args)})
		return fake.output(args)
	}
	return top, lane, &calls
}

func (fake repairFake) output(args []string) (string, error) {
	switch strings.Join(args, " ") {
	case fake.fail:
		return "", errors.New("run /abs/bin/harness " + fake.fail + ": exit status 1")
	case "worktree create autofix-abcdef0":
		return "created lane\n" + fake.lane + "\n", nil
	case "symbolic-ref --short HEAD":
		return "worktree-autofix-abcdef0\n", nil
	case "status --porcelain=v1 -z":
		if fake.changed {
			return " M sample.go\x00 M internal/schemas/testdata/writers/example.txt\x00", nil
		}
	case "diff --binary":
		return "binary patch\n", nil
	case "agents diff --check --path sample.go --path internal/schemas/testdata/writers/example.txt":
		if fake.red {
			return redGateFixture, errors.New("gate red")
		}
	}
	if slices.Equal(args[:min(2, len(args))], []string{"apply", "-R"}) {
		return "", checkRepairPatch(args[2])
	}
	return "", nil
}

func checkRepairPatch(file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if string(data) != "binary patch\n" {
		return errors.New("unexpected rollback patch")
	}
	return nil
}

func repairCalled(calls []repairCall, prefix ...string) bool {
	return slices.ContainsFunc(calls, func(call repairCall) bool {
		return len(call.args) >= len(prefix) && slices.Equal(call.args[:len(prefix)], prefix)
	})
}

func TestRepairGreen(t *testing.T) {
	top, lane, calls := fakeRepair(t, true, false)
	findings := []Finding{
		{Class: ClassGofmt, Path: "sample.go"},
		{Class: ClassGofmt, Path: "sample.go"},
		{Class: ClassGofmt, Path: "state/harness/verify/tree-scratch/sample.go"},
		{Class: ClassGofmt, Path: filepath.Join(top, "sample.go")},
		{Class: ClassMarkdownLong, Path: "guide.md"},
		{Class: ClassWriterRows, Path: "schemas_test.go", Message: "example.txt:3: row a is duplicate or unsorted"},
		{Class: ClassWorkflowBlock},
		{Class: ClassComplexity, Path: "sample.go"},
	}
	res, err := Repair(context.Background(), top, repairSHA, findings)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != lane || res.Lane != "autofix-abcdef0" || len(res.Fixed) != 4 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Notice, "gate green; land it: claude-code worktree land autofix-abcdef0") {
		t.Fatalf("notice = %q", res.Notice)
	}
	checkRepairGreenCalls(t, top, *calls)
	data, err := os.ReadFile(filepath.Join(lane, "internal/schemas/testdata/writers/example.txt"))
	if err != nil || string(data) != "a\tfirst\nz\tlast\n" {
		t.Fatalf("sorted rows = %q, %v", data, err)
	}
}

func checkRepairGreenCalls(t *testing.T, top string, calls []repairCall) {
	t.Helper()
	if !repairCalled(calls, "worktree", "commit") || repairCalled(calls, "worktree", "clean") {
		t.Fatalf("calls = %+v", calls)
	}
	for _, call := range calls {
		if call.name == "gofmt" && !slices.Equal(call.args, []string{"-w", "./sample.go"}) {
			t.Fatalf("gofmt args = %v", call.args)
		}
		if call.dir == top && call.args[0] != "branch" && call.args[0] != "worktree" {
			t.Fatalf("fixer ran in live checkout: %+v", call)
		}
	}
}

func TestRepairNoChange(t *testing.T) {
	top, _, calls := fakeRepair(t, false, false)
	res, err := Repair(context.Background(), top, repairSHA, []Finding{{Class: ClassGofmt, Path: "sample.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Lane != "" || !strings.Contains(res.Notice, "fixers changed nothing") {
		t.Fatalf("result = %+v", res)
	}
	if !repairCalled(*calls, "worktree", "clean", "autofix-abcdef0") || repairCalled(*calls, "worktree", "commit") {
		t.Fatalf("calls = %+v", *calls)
	}
}

func TestRepairRedGate(t *testing.T) {
	top, _, calls := fakeRepair(t, true, true)
	res, err := Repair(context.Background(), top, repairSHA, []Finding{{Class: ClassGofmt, Path: "sample.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Lane != "" || !strings.Contains(res.Notice, "gate still red: lint, test") {
		t.Fatalf("result = %+v", res)
	}
	for line := range strings.Lines(redGateFixture) {
		if strings.Contains(res.Notice, strings.TrimSpace(line)) {
			t.Fatalf("notice carries gate output %q: %q", line, res.Notice)
		}
	}
	if !repairCalled(*calls, "apply", "-R") || !repairCalled(*calls, "worktree", "clean", "autofix-abcdef0") {
		t.Fatalf("calls = %+v", *calls)
	}
	if repairCalled(*calls, "worktree", "commit") {
		t.Fatal("red gate committed")
	}
}

// an expired repair context still cleans the lane: cleanup runs on its own context.
func TestDropLaneCancelledContext(t *testing.T) {
	top, _, calls := fakeRepair(t, false, false)
	inner := run
	run = func(ctx context.Context, dir, name string, args ...string) (string, error) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return inner(ctx, dir, name, args...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, _ := dropLane(ctx, top, "harness", Result{Lane: "autofix-abcdef0", Notice: "fixer failed"}, nil)
	if !repairCalled(*calls, "worktree", "clean", "autofix-abcdef0") || !strings.HasSuffix(res.Notice, "lane removed") {
		t.Fatalf("notice = %q calls = %+v", res.Notice, *calls)
	}
}

// every Repair error after the lane exists removes it, or names it in the notice when the clean fails.
func TestRepairErrorPathsDropLane(t *testing.T) {
	tests := []struct {
		name, fail, notice string
		changed            bool
	}{
		{"lane check", "symbolic-ref --short HEAD", "lane check failed; lane removed", true},
		{"lane status", "status --porcelain=v1 -z", "lane status failed; lane removed", true},
		{"commit refused", "worktree commit", "commit refused; lane removed", true},
		{"clean fails", "worktree clean autofix-abcdef0", "lane autofix-abcdef0 left, clean failed", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fail := tc.fail
			if fail == "worktree commit" {
				fail = "worktree commit -m chore(autofix): repair gofmt found on abcdef0 -- sample.go " +
					"internal/schemas/testdata/writers/example.txt"
			}
			top, _, calls := fakeRepairFailing(t, tc.changed, false, fail)
			res, err := Repair(context.Background(), top, repairSHA, []Finding{{Class: ClassGofmt, Path: "sample.go"}})
			if res.Lane != "" || !strings.Contains(res.Notice, tc.notice) {
				t.Fatalf("result = %+v, %v", res, err)
			}
			if !repairCalled(*calls, "worktree", "clean", "autofix-abcdef0") {
				t.Fatalf("lane not cleaned: %+v", *calls)
			}
		})
	}
}
