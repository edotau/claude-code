package waves

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// verifyRepo makes a git repo holding files (path to content), committed once, and returns its root.
func verifyRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	dir := t.TempDir()
	verifyGit(t, dir, "init", "-q")
	for p, body := range files {
		verifyWrite(t, dir, p, body)
	}
	verifyGit(t, dir, "add", ".")
	verifyGit(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "base")
	return dir
}

func verifyGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func verifyWrite(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// verifyDiff is a Diff stand-in that prints rep as JSON and returns exit.
func verifyDiff(rep agent.DiffReport, exit int, got *agent.DiffOptions) func(agent.DiffOptions) int {
	return func(o agent.DiffOptions) int {
		if got != nil {
			*got = o
		}
		_ = json.NewEncoder(o.Out).Encode(rep)
		return exit
	}
}

// verifyOutside asserts CheckUnion exits CheckExitOutside (or 0 when want is nil) with exactly the want paths outside.
func verifyOutside(t *testing.T, in CheckUnionInput, label string, want []string) {
	t.Helper()
	res, err := CheckUnion(CheckEnv{}, in)
	wantExit := 0
	if want != nil {
		wantExit = CheckExitOutside
	}
	if err != nil || res.Exit != wantExit || (want == nil && len(res.Outside) != 0) ||
		(want != nil && !slices.Equal(res.Outside, want)) {
		t.Fatalf("%s: exit %d outside %v err %v", label, res.Exit, res.Outside, err)
	}
}

func TestVerifyUnionOutsideOnlyWhenFingerprintChanged(t *testing.T) {
	dir := verifyRepo(t, map[string]string{"in/a.txt": "a\n", "dirty/pre.txt": "p\n", "other/o.txt": "o\n"})
	verifyWrite(t, dir, "in/a.txt", "a2\n")
	verifyWrite(t, dir, "dirty/pre.txt", "p2\n")
	fp, err := checkHashObject(dir, "dirty/pre.txt")
	if err != nil || fp == "" {
		t.Fatalf("fingerprint: %q %v", fp, err)
	}
	in := CheckUnionInput{Dir: dir, Packages: []CheckPackage{{ID: "1.1", Owns: []string{"in"}}}, Launch: map[string]string{"dirty/pre.txt": fp}}

	verifyOutside(t, in, "unchanged pre-existing dirt", nil)
	verifyWrite(t, dir, "dirty/pre.txt", "p3\n")
	verifyOutside(t, in, "changed pre-existing file", []string{"dirty/pre.txt"})
	verifyWrite(t, dir, "dirty/pre.txt", "p2\n")
	verifyWrite(t, dir, "other/o.txt", "o2\n")
	verifyOutside(t, in, "clean-at-launch file edited", []string{"other/o.txt"})
}

func TestVerifyUnionGateFailureAndScope(t *testing.T) {
	var got agent.DiffOptions
	rep := agent.DiffReport{Gates: []agent.GateResult{{Name: "go test", Status: "FAIL"}}, Outside: []string{"x.go"}}
	diff := verifyDiff(rep, 1, nil)
	env := CheckEnv{Diff: func(o agent.DiffOptions) int {
		if o.Check {
			got = o
		}
		return diff(o)
	}}
	res, err := CheckUnion(env, CheckUnionInput{
		Dir: "/r", Base: "abc", Packages: []CheckPackage{{ID: "1.1", Owns: []string{"a.go", "b_test.go", "c.md"}}}})
	if err != nil || res.Exit != CheckExitGate {
		t.Fatalf("exit %d err %v, want %d", res.Exit, err, CheckExitGate)
	}
	if !got.Check || got.Base != "abc" || !got.JSON {
		t.Fatalf("diff options %+v", got)
	}
	want := []string{"a.go", "b_test.go", "c.md", "a_test.go"}
	if !slices.Equal(got.Paths, want) {
		t.Fatalf("paths %v, want %v", got.Paths, want)
	}
	if _, err := CheckUnion(CheckEnv{}, CheckUnionInput{Dir: "/r"}); err == nil {
		t.Fatal("empty union must be an error")
	}
	usage := CheckEnv{Diff: func(o agent.DiffOptions) int { io.WriteString(o.Err, "bad path\n"); return agent.DiffExitUsage }}
	if _, err := CheckUnion(usage, CheckUnionInput{Dir: "/r", Packages: []CheckPackage{{ID: "1.1", Owns: []string{"a"}}}}); err == nil ||
		!strings.Contains(err.Error(), "bad path") {
		t.Fatalf("usage exit error = %v", err)
	}
}

// verifyPackages is a Diff stand-in: the union call (Check) reports union, every other call the report of its Verify id.
func verifyPackages(union agent.DiffReport, byID map[string]agent.DiffReport) func(agent.DiffOptions) int {
	return func(o agent.DiffOptions) int {
		rep := union
		if !o.Check {
			rep = byID[o.Verify]
		}
		_ = json.NewEncoder(o.Out).Encode(rep)
		return 0
	}
}

// verifyWindow is the report of a dispatch that ran from sec start to sec end and saw outside as outside its scope.
func verifyWindow(start, end int, outside ...string) agent.DiffReport {
	at := func(sec int) string { return time.Unix(1_700_000_000+int64(sec), 0).UTC().Format(time.RFC3339) }
	return agent.DiffReport{Outside: outside, Verify: &agent.VerifyReport{Start: at(start), End: at(end)}}
}

func verifyModTimes(sec int) func(string, string) (time.Time, error) {
	return func(string, string) (time.Time, error) {
		if sec < 0 {
			return time.Time{}, nil
		}
		return time.Unix(1_700_000_000+int64(sec), 0), nil
	}
}

func TestVerifyEachPackageIsCheckedAgainstItsOwnFiles(t *testing.T) {
	pkgs := []CheckPackage{{ID: "1.1", Dispatch: "a", Owns: []string{"a.go"}}, {ID: "1.2", Dispatch: "b", Owns: []string{"b.go"}}}
	run := func(a, b agent.DiffReport, mtime int, pkgs []CheckPackage) CheckUnionResult {
		t.Helper()
		env := CheckEnv{Diff: verifyPackages(agent.DiffReport{}, map[string]agent.DiffReport{"a": a, "b": b}),
			ModTime: verifyModTimes(mtime)}
		res, err := CheckUnion(env, CheckUnionInput{Dir: "/r", Packages: pkgs})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// b.go changed at sec 50, after package 1.2 ended (sec 30) and while package 1.1 ran: 1.1 wrote it.
	res := run(verifyWindow(40, 60, "b.go"), verifyWindow(0, 30), 50, pkgs)
	want := CheckViolation{Package: "1.1", File: "b.go", Owners: []string{"1.2"}}
	if res.Exit != CheckExitOutside || len(res.Violations) != 1 || !slices.Equal(res.Violations[0].Owners, want.Owners) ||
		res.Violations[0].Package != want.Package || res.Violations[0].File != want.File || len(res.Unattributed) != 0 {
		t.Fatalf("cross-package write: %+v", res)
	}
	// The same change inside its owner's own window is the owner's write, even while the other package ran.
	scopedRival := []CheckPackage{{ID: "1.1", Dispatch: "a", Owns: []string{"a.go"}, Scoped: true}, pkgs[1]}
	res = run(verifyWindow(0, 60, "b.go"), verifyWindow(40, 60), 50, scopedRival)
	if res.Exit != 0 || len(res.Violations)+len(res.Unattributed) != 0 {
		t.Fatalf("owner write: %+v", res)
	}
	// With a rival that was not refused the file running in the same window, the writer cannot be told: exit 3.
	res = run(verifyWindow(0, 60, "b.go"), verifyWindow(40, 60), 50, pkgs)
	if res.Exit != CheckExitOutside || !slices.Equal(res.Unattributed, []string{"b.go"}) || len(res.Violations) != 0 {
		t.Fatalf("parallel unscoped rival: %+v", res)
	}
	// No window for the owner, or no write time, or no package running then: unattributed, never passed.
	noOwnerRun := []CheckPackage{pkgs[0], {ID: "1.2", Owns: []string{"b.go"}}}
	for label, got := range map[string]CheckUnionResult{
		"owner without dispatch": run(verifyWindow(40, 60, "b.go"), agent.DiffReport{}, 50, noOwnerRun),
		"nobody ran then":        run(verifyWindow(40, 60, "b.go"), verifyWindow(0, 30), 90, pkgs),
		"file gone":              run(verifyWindow(40, 60, "b.go"), verifyWindow(0, 30), -1, pkgs),
	} {
		if got.Exit != CheckExitOutside || !slices.Equal(got.Unattributed, []string{"b.go"}) || len(got.Violations) != 0 {
			t.Errorf("%s: %+v", label, got)
		}
	}
}

func TestVerifyPackageWithoutOwnsIsAnError(t *testing.T) {
	_, err := CheckUnion(CheckEnv{}, CheckUnionInput{Dir: "/r", Packages: []CheckPackage{{ID: "1.1"}}})
	if err == nil || !strings.Contains(err.Error(), "1.1") {
		t.Fatalf("package without owns: %v", err)
	}
}

func TestVerifyRoutingProof(t *testing.T) {
	var got agent.DiffOptions
	rep := agent.DiffReport{Verify: &agent.VerifyReport{ID: "job-1", Agent: "opencode", Model: "m", Route: "prod",
		Mismatch: true, Mismatches: []string{"model: asked for \"x\", ran \"m\""}}}
	res, err := RoutingProof(CheckEnv{Diff: verifyDiff(rep, 1, &got)}, "/r", "job-1")
	if err != nil || !res.Mismatch || res.Agent != "opencode" || len(res.Mismatches) != 1 {
		t.Fatalf("result %+v err %v", res, err)
	}
	if got.Verify != "job-1" || got.Check {
		t.Fatalf("diff options %+v: want Verify set and no gates", got)
	}
	rep.Verify.Mismatch = false
	if res, err = RoutingProof(CheckEnv{Diff: verifyDiff(rep, 0, nil)}, "/r", "job-1"); err != nil || res.Mismatch {
		t.Fatalf("clean routing: %+v %v", res, err)
	}
	if _, err = RoutingProof(CheckEnv{Diff: verifyDiff(agent.DiffReport{}, 0, nil)}, "/r", "job-2"); err == nil {
		t.Fatal("a report without a verify section must be an error")
	}
}

func TestVerifyGateRefusesGitWrites(t *testing.T) {
	for _, cmd := range []string{"git commit -m x", "go test ./... && git add .", "git -C /tmp/r push", "git --no-pager stash",
		"cd x; git checkout -- .", "xargs git reset", "git -c a=b rebase main", "git clean -fd", "git merge x", "git restore ."} {
		if GateRefusal(cmd) == nil {
			t.Errorf("%q must be refused", cmd)
		}
	}
	for _, cmd := range []string{"go test ./internal/...", "git diff --stat", "git log --grep commit", "git status --short",
		"git diff HEAD -- docs/add.md", "make lint"} {
		if err := GateRefusal(cmd); err != nil {
			t.Errorf("%q refused: %v", cmd, err)
		}
	}
	ran := false
	env := CheckEnv{Exec: func(context.Context, string, string, []string, io.Writer) (int, error) { ran = true; return 0, nil }}
	out := filepath.Join(t.TempDir(), "gate.txt")
	if _, err := GateRun(context.Background(), env, GateInput{Cmd: "git commit -m x", OutPath: out}); err == nil || ran {
		t.Fatalf("refused gate: err %v, ran %v", err, ran)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("refused gate must not create its output file")
	}
}

func verifyGateTail(t *testing.T, res GateResult) {
	t.Helper()
	if len(res.Tail) != GateTailLines || res.Tail[0] != "63" || res.Tail[len(res.Tail)-2] != "base=abc123" ||
		res.Tail[len(res.Tail)-1] != "last" {
		t.Fatalf("tail %d lines %q..%q", len(res.Tail), res.Tail[0], res.Tail[len(res.Tail)-2:])
	}
	full, err := os.ReadFile(res.Saved)
	if err != nil || strings.Count(string(full), "\n") != 101 || !strings.HasPrefix(string(full), "1\n") {
		t.Fatalf("saved output: %d lines, err %v", strings.Count(string(full), "\n"), err)
	}
}

func TestVerifyGateRunTailAndExit(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "sub", "wave-1.txt")
	res, err := GateRun(context.Background(), CheckEnv{}, GateInput{Dir: dir, OutPath: out, Env: []string{"BASE=abc123"},
		Cmd: "seq 1 100; echo \"base=$BASE\" >&2; printf last; exit 5"})
	if err != nil || res.Exit != 5 {
		t.Fatalf("exit %d err %v, want 5", res.Exit, err)
	}
	verifyGateTail(t, res)
	if ok, err := GateRun(context.Background(), CheckEnv{}, GateInput{Dir: dir, OutPath: out, Cmd: "echo 1"}); err != nil ||
		ok.Exit != 0 || !slices.Equal(ok.Tail, []string{"1"}) {
		t.Fatalf("passing gate: %+v %v", ok, err)
	}
}

func TestVerifyNoOwnerFileNamesTheOnlyPackageRunning(t *testing.T) {
	run := func(a, b agent.DiffReport) CheckUnionResult {
		t.Helper()
		union := agent.DiffReport{Outside: []string{"c.go"}}
		env := CheckEnv{Diff: verifyPackages(union, map[string]agent.DiffReport{"a": a, "b": b}), ModTime: verifyModTimes(50)}
		res, err := CheckUnion(env, CheckUnionInput{Dir: "/r", Packages: []CheckPackage{
			{ID: "1.1", Dispatch: "a", Owns: []string{"a.go"}}, {ID: "1.2", Dispatch: "b", Owns: []string{"b.go"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := run(verifyWindow(40, 60, "c.go"), verifyWindow(0, 30, "c.go"))
	if res.Exit != CheckExitOutside || !slices.Equal(res.Outside, []string{"c.go"}) || len(res.Violations) != 1 ||
		res.Violations[0].Package != "1.1" || res.Violations[0].File != "c.go" || len(res.Violations[0].Owners) != 0 {
		t.Fatalf("one package running: %+v", res)
	}
	if res = run(verifyWindow(40, 60, "c.go"), verifyWindow(40, 60, "c.go")); len(res.Violations) != 0 || res.Exit != CheckExitOutside {
		t.Fatalf("two packages running must stay unnamed: %+v", res)
	}
}
