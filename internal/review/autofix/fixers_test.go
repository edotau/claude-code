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

const repairSHA = "abcdef0123456789"

type repairCall struct {
	dir, name string
	args      []string
}

type repairFake struct {
	lane         string
	changed, red bool
}

func fakeRepair(t *testing.T, changed, red bool) (string, string, *[]repairCall) {
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
	oldRun, oldBin := run, harnessBin
	t.Cleanup(func() { run, harnessBin = oldRun, oldBin })
	harnessBin = func() (string, error) { return "harness", nil }
	var calls []repairCall
	fake := repairFake{lane, changed, red}
	run = func(_ context.Context, dir, name string, args ...string) (string, error) {
		calls = append(calls, repairCall{dir, name, slices.Clone(args)})
		return fake.output(args)
	}
	return top, lane, &calls
}

func (fake repairFake) output(args []string) (string, error) {
	switch strings.Join(args, " ") {
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
			return "FAIL test: still broken\n", errors.New("gate red")
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
	if !strings.Contains(res.Notice, "gate green; land it: claude-code worktree merge autofix-abcdef0") {
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
	if res.Lane != "" || !strings.Contains(res.Notice, "gate still red: FAIL test: still broken") {
		t.Fatalf("result = %+v", res)
	}
	if !repairCalled(*calls, "apply", "-R") || !repairCalled(*calls, "worktree", "clean", "autofix-abcdef0") {
		t.Fatalf("calls = %+v", *calls)
	}
	if repairCalled(*calls, "worktree", "commit") {
		t.Fatal("red gate committed")
	}
}
