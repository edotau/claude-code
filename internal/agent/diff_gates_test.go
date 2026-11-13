package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/review/source"
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

// branchy returns a Go function whose cyclomatic complexity is n+1.
func branchy(name string, n int) string {
	return "func " + name + "(x int) int {\n\tr := 0\n" + strings.Repeat("\tif x > 1 {\n\t\tr++\n\t}\n", n) + "\treturn r\n}\n"
}

// TestComplexityGateOnlyFailsIntroducedFindings runs the real detector on two versions of one file.
func TestComplexityGateOnlyFailsIntroducedFindings(t *testing.T) {
	const head = "package pkg\n\n"
	base := head + branchy("Old", 12)
	(&fakeGates{}).install(t)
	gateSimplify = source.RunSimplify
	gateBlobs = func(context.Context, string, string, []string) (map[string]string, error) {
		return map[string]string{"pkg/p.go": base}, nil
	}
	dir, files := gateRepo(t)
	for name, tc := range map[string]struct{ body, want, notWant string }{
		"edited elsewhere": {base + "\nfunc Small() int { return 1 }\n", "", ""},
		"new function":     {base + "\n" + branchy("Fresh", 12), "Fresh", "Old"},
		"got worse":        {head + branchy("Old", 16), "Old", ""},
	} {
		writeRepoFile(t, dir, "pkg/p.go", tc.body)
		out, _, err := complexityGate(context.Background(), newGateInput(dir, "HEAD", files))
		if (tc.want == "") != (err == nil) || !strings.Contains(out, tc.want) || (tc.notWant != "" && strings.Contains(out, tc.notWant)) {
			t.Errorf("%s: err %v, output %q; want findings naming %q and not %q", name, err, out, tc.want, tc.notWant)
		}
	}
	untracked := []DiffFile{{Path: "pkg/p.go", Status: "?"}}
	writeRepoFile(t, dir, "pkg/p.go", base)
	if out, _, err := complexityGate(context.Background(), newGateInput(dir, "HEAD", untracked)); err == nil || !strings.Contains(out, "Old") {
		t.Errorf("a new file must report all its findings: err %v, output %q", err, out)
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

func TestGitBlobsReadsBaseInOneCall(t *testing.T) {
	dir := newDiffRepo(t)
	got, err := gitBlobs(context.Background(), dir, "HEAD", []string{"pkg/p.go", "gone.go", "other/o.txt"})
	if err != nil || len(got) != 2 || got["pkg/p.go"] != "package pkg\n\nfunc P() int { return 1 }\n" || got["other/o.txt"] != "one\n" {
		t.Errorf("gitBlobs = %q, %v; want the two tracked files and no entry for gone.go", got, err)
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
