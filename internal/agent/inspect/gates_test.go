package inspect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeGates struct {
	calls   []string
	fail    map[string]string // gate command name+first arg -> output, which also fails the call
	lint    bool
	simpOut string
}

// install swaps the gate seams for fakes and restores them when the test ends.
func (f *fakeGates) install(t *testing.T) {
	t.Helper()
	origRun, origLook, origSimp, origBlobs := gateRun, gateLookPath, gateSimplify, gateBlobs
	t.Cleanup(func() { gateRun, gateLookPath, gateSimplify, gateBlobs = origRun, origLook, origSimp, origBlobs })
	gateBlobs = func(context.Context, string, string, []string) (map[string]string, error) { return nil, nil }
	gateRun = func(_ context.Context, _, name string, args ...string) (string, error) {
		key := name + " " + args[0]
		f.calls = append(f.calls, key)
		if out, bad := f.fail[key]; bad {
			return out, errors.New("exit status 1")
		}
		return "", nil
	}
	gateLookPath = func(string) (string, error) {
		if !f.lint {
			return "", exec.ErrNotFound
		}
		return "/bin/golangci-lint", nil
	}
	gateSimplify = func(_ []string, stdout, _ io.Writer) int {
		_, _ = io.WriteString(stdout, cmpOr(f.simpOut, `{"files":[],"summary":{}}`))
		return 0
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func gateRepo(t *testing.T) (string, []DiffFile) {
	t.Helper()
	dir := t.TempDir()
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n")
	return dir, []DiffFile{{Path: "pkg/p.go", Status: "M"}}
}

func gateStatuses(rs []GateResult) string {
	var parts []string
	for _, r := range rs {
		parts = append(parts, r.Status+" "+r.Name)
	}
	return strings.Join(parts, ", ")
}

func TestGatesSkipAfterBuildFailure(t *testing.T) {
	dir, files := gateRepo(t)
	f := &fakeGates{lint: true, fail: map[string]string{"go build": dir + "/pkg/p.go:3:1: undefined: x\n"}}
	f.install(t)
	rs := runGates(context.Background(), dir, "HEAD", files)
	want := "ok gofmt, FAIL build, skip vet, skip golangci-lint, ok complexity, skip test"
	if got := gateStatuses(rs); got != want {
		t.Fatalf("gates = %s, want %s", got, want)
	}
	if rs[1].OutputHead != "pkg/p.go:3:1: undefined: x" || rs[2].Reason != "build failed" {
		t.Errorf("build output %q (root must be scrubbed), vet reason %q", rs[1].OutputHead, rs[2].Reason)
	}
	if slices.Contains(f.calls, "go vet") || slices.Contains(f.calls, "go test") {
		t.Errorf("gates after a failed build ran anyway: %v", f.calls)
	}
	if diffExit(DiffReport{Gates: rs}) != DiffExitGate {
		t.Errorf("a failed build must exit 1")
	}
}

func TestGatesCutLongFailureAndRunTheRest(t *testing.T) {
	dir, files := gateRepo(t)
	var long []string
	for i := range 45 {
		long = append(long, fmt.Sprintf("vet line %d", i))
	}
	f := &fakeGates{fail: map[string]string{"go vet": strings.Join(long, "\n") + "\n"}}
	f.install(t)
	rs := runGates(context.Background(), dir, "HEAD", files)
	if got, want := gateStatuses(rs), "ok gofmt, ok build, FAIL vet, skip golangci-lint, ok complexity, ok test"; got != want {
		t.Fatalf("gates = %s, want %s", got, want)
	}
	if n := strings.Count(rs[2].OutputHead, "\n") + 1; n != diffHeadLines || rs[2].CutLines != 15 {
		t.Errorf("vet kept %d lines, cut %d; want 30 kept, 15 cut", n, rs[2].CutLines)
	}
	if rs[3].Reason != "not installed" || diffExit(DiffReport{Gates: rs}) != DiffExitGate {
		t.Errorf("lint reason %q, exit %d", rs[3].Reason, diffExit(DiffReport{Gates: rs}))
	}
}

func TestGatesDocsOnlySkipGoGates(t *testing.T) {
	dir, _ := gateRepo(t)
	(&fakeGates{lint: true}).install(t)
	docs := runGates(context.Background(), dir, "HEAD", []DiffFile{{Path: "README.md", Status: "M"}})
	if got := gateStatuses(docs); got != "skip go gates, ok docs" || !strings.Contains(docs[0].Reason, "make lint") {
		t.Errorf("docs-only change: %+v", docs)
	}
}

func TestScrubPathsNamesTempAndHome(t *testing.T) {
	t.Setenv("TMPDIR", "/scratch/tmp")
	t.Setenv("HOME", "/home/u")
	in := "/repo/pkg/a.go:1\n/scratch/tmp/go-build7/b.go:2\n/home/u/go/mod/c.go:3\n/elsewhere/d.go:4"
	want := "pkg/a.go:1\n<tmp>/go-build7/b.go:2\n~/go/mod/c.go:3\n/elsewhere/d.go:4"
	if got := scrubPaths(in, "/repo"); got != want {
		t.Errorf("scrubPaths =\n%s\nwant\n%s", got, want)
	}
}

func TestDocsGateEndsOptionsAndChunks(t *testing.T) {
	origRun, origLook := gateRun, gateLookPath
	t.Cleanup(func() { gateRun, gateLookPath = origRun, origLook })
	gateLookPath = func(string) (string, error) { return "/bin/mdsmith", nil }
	var calls [][]string
	gateRun = func(_ context.Context, _, _ string, args ...string) (string, error) {
		calls = append(calls, args)
		return "", nil
	}
	files := []string{"-notes.md"}
	for i := range 450 {
		files = append(files, fmt.Sprintf("d/f%d.md", i))
	}
	if _, _, err := docsGate(context.Background(), gateInput{root: t.TempDir(), mds: files}); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, args := range calls {
		if len(args) < 3 || args[2] != "--" || len(args)-3 > docsGateChunk {
			t.Fatalf("bad call %v...", args[:min(len(args), 5)])
		}
		total += len(args) - 3
	}
	if len(calls) != 3 || total != len(files) || calls[0][3] != "-notes.md" {
		t.Fatalf("calls=%d files=%d, want 3 calls covering %d", len(calls), total, len(files))
	}
}

func TestDocsGateRunsOnChangedMarkdownOnly(t *testing.T) {
	dir, files := gateRepo(t)
	f := &fakeGates{lint: true, fail: map[string]string{dir + "/bin/mdsmith check": "a.md:3:121 MDS001 line too long\n"}}
	f.install(t)
	md := []DiffFile{{Path: "a.md", Status: "M"}, {Path: "gone.md", Status: "D"}}
	rs := runGates(context.Background(), dir, "HEAD", append(files, md...))
	if last := rs[len(rs)-1]; last.Name != "docs" || last.Status != "FAIL" || !strings.Contains(last.OutputHead, "MDS001") {
		t.Errorf("docs gate = %+v; want a FAIL carrying the mdsmith line", last)
	}
	if rs = runGates(context.Background(), dir, "HEAD", files); strings.Contains(gateStatuses(rs), "docs") {
		t.Errorf("a Go-only change ran the docs gate: %s", gateStatuses(rs))
	}
	(&fakeGates{}).install(t)
	if rs = runGates(context.Background(), dir, "HEAD", md); rs[len(rs)-1].Reason != "mdsmith not installed" {
		t.Errorf("missing mdsmith: %+v", rs)
	}
}

// The docs gate also runs the memory formatter check on changed bank files, not on bank files the change left alone.
func TestDocsGateChecksChangedMemoryBankFiles(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	long := "- " + strings.TrimSpace(strings.Repeat("word ", 40)) + "\n"
	writeRepoFile(t, cfg, "memory/activeContext.md", "# Focus\n\n"+long)
	writeRepoFile(t, cfg, "memory/"+filepath.Base(cfg)+"--activeContext.md", "# Focus\n\n- short\n")
	changed := func(rel string) []DiffFile { return []DiffFile{{Path: rel, Status: "M"}} }
	dirty, clean := "memory/activeContext.md", "memory/"+filepath.Base(cfg)+"--activeContext.md"

	for _, lint := range []bool{true, false} { // the memory check does not depend on mdsmith being installed
		(&fakeGates{lint: lint}).install(t)
		last := func(rel string) GateResult {
			rs := runGates(context.Background(), cfg, "HEAD", changed(rel))
			return rs[len(rs)-1]
		}
		r := last(dirty)
		if r.Name != "docs" || r.Status != "FAIL" || !strings.Contains(r.OutputHead, dirty) {
			t.Errorf("lint=%t dirty changed bank file: %+v, want a docs FAIL naming the file", lint, r)
		}
		if r := last(clean); r.Status == "FAIL" {
			t.Errorf("lint=%t clean changed bank file failed on another bank file: %+v", lint, r)
		}
	}
}

func TestGatesComplexityFindings(t *testing.T) {
	dir, files := gateRepo(t)
	finding := `{"files":[{"file":"` + dir + `/pkg/p.go","complexity":[{"rule":"cyclomatic-complexity","severity":"warn",` +
		`"line":3,"message":"too branchy"}],"quality":{"smells":[{"type":"magic_number","severity":"low","message":"m",` +
		`"location":"l"},{"type":"long_function","severity":"medium","message":"long","location":"f"}]}}]}`
	(&fakeGates{simpOut: finding}).install(t)
	rs := runGates(context.Background(), dir, "HEAD", files)
	head := rs[4].OutputHead
	if rs[4].Status != "FAIL" || !strings.Contains(head, "pkg/p.go line 3 [warn]") || !strings.Contains(head, "long_function") ||
		strings.Contains(head, "magic_number") || strings.Contains(head, dir) {
		t.Errorf("complexity gate = %s %q; want the warn and medium findings only, relative paths", rs[4].Status, head)
	}
}

// A production Go change adds ./internal/schemas (it pins file writers) to the test gate; a test-only change does not.
func TestTestGateAddsSchemasOnProductionChange(t *testing.T) {
	dir := t.TempDir()
	writeRepoFile(t, dir, "internal/schemas/s.go", "package schemas\n")
	var args []string
	orig := gateRun
	t.Cleanup(func() { gateRun = orig })
	gateRun = func(_ context.Context, _, _ string, a ...string) (string, error) { args = a; return "", nil }
	for _, c := range []struct {
		file string
		want bool
	}{{"pkg/p.go", true}, {"pkg/p_test.go", false}} {
		in := newGateInput(dir, "HEAD", []DiffFile{{Path: c.file, Status: "M"}})
		in.pkgs = []string{"./pkg"}
		if _, _, err := testGate(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		if got := slices.Contains(args, "./internal/schemas"); got != c.want {
			t.Errorf("%s: test args %v, schemas = %v, want %v", c.file, args, got, c.want)
		}
	}
}
