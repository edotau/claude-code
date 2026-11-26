package inspect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/mdlint"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestMain(m *testing.M) {
	gateMdsmith = func(root string) string { // follows the gateLookPath fakes: root/bin first, then the PATH name
		if local := filepath.Join(root, "bin", "mdsmith"); fileFound(local) {
			return local
		}
		if _, err := gateLookPath("mdsmith"); err == nil {
			return "mdsmith"
		}
		return ""
	}
	os.Exit(hosttest.Main(m))
}

type fakeGates struct {
	mu      sync.Mutex
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
		f.mu.Lock()
		f.calls = append(f.calls, key)
		f.mu.Unlock()
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

func TestGatesKeepOwnResultAfterBuildFailure(t *testing.T) {
	dir, files := gateRepo(t)
	f := &fakeGates{lint: true, fail: map[string]string{"go build": dir + "/pkg/p.go:3:1: undefined: x\n"}}
	f.install(t)
	rs := runGates(context.Background(), dir, "HEAD", files)
	want := "ok gofmt, FAIL build, ok vet, ok golangci-lint, ok complexity, ok test, ok quality"
	if got := gateStatuses(rs); got != want {
		t.Fatalf("gates = %s, want %s", got, want)
	}
	if got := rs[1].Findings; len(got) != 1 || got[0].String() != "pkg/p.go:3: build: undefined: x" {
		t.Errorf("build rows %v (root must be scrubbed)", got)
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
	if got, want := gateStatuses(rs), "ok gofmt, ok build, FAIL vet, skip golangci-lint, ok complexity, ok test, ok quality"; got != want {
		t.Fatalf("gates = %s, want %s", got, want)
	}
	rows := rs[2].Findings
	if len(rows) != diffHeadLines+1 || rows[len(rows)-1].String() != "vet: +15 more lines cut" {
		t.Errorf("vet kept %d rows, last %v; want 30 unparsed lines then one +15 row", len(rows), rows[len(rows)-1])
	}
	if rs[3].Reason != "golangci-lint not installed (make install DEV_TOOLS=1)" || diffExit(DiffReport{Gates: rs}) != DiffExitGate {
		t.Errorf("lint reason %q, exit %d", rs[3].Reason, diffExit(DiffReport{Gates: rs}))
	}
}

func TestGatesDocsOnlySkipGoGates(t *testing.T) {
	dir, _ := gateRepo(t)
	(&fakeGates{lint: true}).install(t)
	docs := runGates(context.Background(), dir, "HEAD", []DiffFile{{Path: "README.md", Status: "M"}})
	if got := gateStatuses(docs); got != "skip go gates, ok docs, ok spell" || !strings.Contains(docs[0].Reason, "agents diff") {
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
	if _, _, err := docsGate(context.Background(), gateInput{root: t.TempDir(), files: files}); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, args := range calls {
		if len(args) < 3 || args[2] != "--" || len(args)-3 > fileChunk {
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
	f := &fakeGates{lint: true, fail: map[string]string{dir + "/bin/mdsmith check": "a.md:3:1 MDS008 multiple consecutive blank lines\n"}}
	f.install(t)
	md := []DiffFile{{Path: "a.md", Status: "M"}, {Path: "gone.md", Status: "D"}}
	rs := runGates(context.Background(), dir, "HEAD", append(files, md...))
	if last := rs[len(rs)-2]; last.Name != "docs" || last.Status != "FAIL" || len(last.Findings) != 1 || last.Findings[0].String() != "a.md:3: mdsmith/MDS008: multiple consecutive blank lines" {
		t.Errorf("docs gate = %+v; want a FAIL with the mdsmith row", last)
	}
	if rs = runGates(context.Background(), dir, "HEAD", files); strings.Contains(gateStatuses(rs), "docs") {
		t.Errorf("a Go-only change ran the docs gate: %s", gateStatuses(rs))
	}
	(&fakeGates{}).install(t)
	if rs = runGates(context.Background(), dir, "HEAD", md); rs[len(rs)-2].Reason != "mdsmith not installed (make mdsmith)" {
		t.Errorf("missing mdsmith: %+v", rs)
	}
}

// A config-only change runs the real parity check; drift (MD013 line_length 100) fails the docs gate with Rule parity.
func TestDocsGateParity(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	writeRepoFile(t, cfg, "bin/mdsmith", "#!/bin/sh\ncase \"$1\" in\ninit) echo 'note: MD010 option \"code_blocks\": none' >&2 ;;\n"+
		"kinds) echo '{\"rules\":{\"line-length\":{\"leaves\":[{\"path\":\"max\",\"value\":120}]}}}' ;;\nesac\n")
	if err := os.Chmod(filepath.Join(cfg, "bin", "mdsmith"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeRepoFile(t, root, mdlint.RulesFile, `{"MD013":{"line_length":100,"stern":true},"MD010":{"code_blocks":false}}`)
	writeRepoFile(t, root, mdlint.CLI2File, `{"ignores":["internal/**/testdata/**"]}`)
	writeRepoFile(t, root, mdlint.MdsmithFile, "files:\n  - '**/*.md'\nignore:\n  - 'internal/**/testdata/**'\n")
	writeRepoFile(t, root, "README.md", "# Parity\n")
	(&fakeGates{lint: true}).install(t)
	rs := runGates(context.Background(), root, "HEAD", []DiffFile{{Path: mdlint.RulesFile, Status: "M"}})
	r := rs[len(rs)-2]
	if r.Name != "docs" || r.Status != "FAIL" || len(r.Findings) != 1 || r.Findings[0].Rule != "parity" ||
		r.Findings[0].Path != mdlint.RulesFile || !strings.Contains(r.Findings[0].Message, "MD013.line_length: expected 120") {
		t.Fatalf("docs gate = %+v; want one parity FAIL on MD013.line_length", r)
	}
	writeRepoFile(t, root, mdlint.RulesFile, `{"MD013":{"line_length":120,"stern":true},"MD010":{"code_blocks":false}}`)
	rs = runGates(context.Background(), root, "HEAD", []DiffFile{{Path: "README.md", Status: "M"}, {Path: mdlint.RulesFile, Status: "M"}})
	if r = rs[len(rs)-2]; r.Status != "ok" {
		t.Errorf("clean parity: %+v, want ok", r)
	}
	if in := newGateInput(root, "HEAD", []DiffFile{{Path: mdlint.CLI2File, Status: "D"}}); !parityDue(in) {
		t.Error("a deleted lint config must trigger parity")
	}
	if in := newGateInput(t.TempDir(), "HEAD", []DiffFile{{Path: mdlint.RulesFile, Status: "M"}}); parityDue(in) {
		t.Error("a repo without .mdsmith.yml must not run parity")
	}
	if err := os.Remove(filepath.Join(cfg, "bin", "mdsmith")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	rs = runGates(context.Background(), root, "HEAD", []DiffFile{{Path: mdlint.MdsmithFile, Status: "M"}})
	if r = rs[len(rs)-2]; r.Status != "skip" || r.Reason != "mdsmith not installed (make mdsmith)" {
		t.Errorf("no mdsmith: %+v, want the install skip", r)
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
			return rs[len(rs)-2]
		}
		r := last(dirty)
		if r.Name != "docs" || r.Status != "FAIL" || len(r.Findings) != 1 || r.Findings[0].Path != dirty {
			t.Errorf("lint=%t dirty changed bank file: %+v, want a docs FAIL naming the file", lint, r)
		}
		if r := last(clean); r.Status == "FAIL" {
			t.Errorf("lint=%t clean changed bank file failed on another bank file: %+v", lint, r)
		}
		if r := last(clean); !lint && r.Status != "skip" {
			t.Errorf("mdsmith missing, memory check clean: %+v, want skip, not ok", r)
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
	var rows []string
	for _, f := range rs[4].Findings {
		rows = append(rows, f.String())
	}
	head := strings.Join(rows, "\n")
	if rs[4].Status != "FAIL" || !strings.Contains(head, "pkg/p.go:3: complexity/cyclomatic-complexity: warn too branchy") ||
		!strings.Contains(head, "long_function") || strings.Contains(head, "magic_number") || strings.Contains(head, dir) {
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

// --all on the live config dir checks the whole memory bank, as make lint-docs does, though it is untracked.
func TestDocsGateAllChecksWholeBankOnLiveDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	long := "- " + strings.TrimSpace(strings.Repeat("word ", 40)) + "\n"
	writeRepoFile(t, cfg, "memory/activeContext.md", "# Focus\n\n"+long)
	writeRepoFile(t, cfg, "README.md", "# R\n")
	(&fakeGates{lint: true}).install(t)
	in := newGateInput(cfg, "HEAD", []DiffFile{{Path: "README.md", Status: "M"}})
	if out, err := memoryFmtGate(in); err != nil || out != "" {
		t.Fatalf("changed-files mode: out %q err %v; want the unchanged bank left alone", out, err)
	}
	in.whole = true
	if out, err := memoryFmtGate(in); err == nil || !strings.Contains(out, "activeContext.md: memory fmt --check") {
		t.Errorf("whole mode: out %q err %v; want the dirty bank file reported", out, err)
	}
}

// HARNESS_COMPLEXITY_GATE=recommended shows a function's complexity once, as advisory, and does not fail; strict fails.
func TestGatesComplexityRecommendedAdvisory(t *testing.T) {
	dir, files := gateRepo(t)
	out := `{"files":[{"file":"` + dir + `/pkg/p.go","complexity":[{"rule":"cyclomatic-complexity","severity":"warn",` +
		`"line":3,"message":"'f' has cyclomatic complexity 12 (max 10)."}],"quality":{"smells":[{"type":"high_complexity",` +
		`"severity":"medium","message":"Function 'f' has complexity 12 (max: 10)","location":"f"}]}}]}`
	(&fakeGates{simpOut: out}).install(t)
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Env, "HARNESS_COMPLEXITY_GATE", "recommended")
	rs := runGates(context.Background(), dir, "HEAD", files)
	if rs[4].Status != "ok" || rs[4].Reason != "advisory" || len(rs[4].Findings) != 1 {
		t.Errorf("recommended = %s %q %v; want ok advisory with one finding", rs[4].Status, rs[4].Reason, rs[4].Findings)
	}
	vaulttest.Put(t, vault.Env, "HARNESS_COMPLEXITY_GATE", "strict")
	if rs = runGates(context.Background(), dir, "HEAD", files); rs[4].Status != "FAIL" {
		t.Errorf("strict = %s; want FAIL", rs[4].Status)
	}
}
