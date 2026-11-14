package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

func writeRepoFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)
	if _, err := proc.RunGit(context.Background(), dir, full...); err != nil {
		t.Fatal(err)
	}
}

// newDiffRepo commits pkg/p.go, pkg/p_test.go and other/o.txt in a fresh repo.
func newDiffRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repoGit(t, dir, "init", "-q")
	writeRepoFile(t, dir, "go.mod", "module x\n\ngo 1.22\n")
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n\nfunc P() int { return 1 }\n")
	writeRepoFile(t, dir, "pkg/p_test.go", "package pkg\n")
	writeRepoFile(t, dir, "other/o.txt", "one\n")
	repoGit(t, dir, "add", "-A")
	repoGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func runDiff(t *testing.T, o DiffOptions) (code int, out, errOut string) {
	t.Helper()
	var so, se bytes.Buffer
	o.Out, o.Err = &so, &se
	return Diff(o), so.String(), se.String()
}

// squeeze collapses runs of spaces so assertions do not depend on column padding.
func squeeze(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}

func TestDiffListsChangesAndOutsidePath(t *testing.T) {
	dir := newDiffRepo(t)
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n\nfunc P() int { return 2 }\n\nfunc Q() int { return 3 }\n")
	writeRepoFile(t, dir, "pkg/new_test.go", "package pkg\n\n// t\n")
	writeRepoFile(t, dir, "other/extra.md", "x\ny\n")
	repoGit(t, dir, "mv", "other/o.txt", "other/o r.txt")

	code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Paths: []string{"pkg"}})
	if code != DiffExitOutside || errOut != "" {
		t.Fatalf("exit %d stderr %q, want 3 and none\n%s", code, errOut, out)
	}
	for _, want := range []string{
		"? +2 -0 other/extra.md\n",
		"R +0 -0 other/o.txt -> other/o r.txt\n",
		"? +3 -0 pkg/new_test.go (test)\n",
		"M +3 -1 pkg/p.go\n",
		"outside --path (2):\nother/extra.md\nother/o r.txt\n",
		"agents diff: 4 files +8 -1 (prod +5 -1, test +3 -0); 2 outside --path\n",
	} {
		if !strings.Contains(squeeze(out), want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}

	code, jsonOut, _ := runDiff(t, DiffOptions{Dir: dir, Paths: []string{"pkg"}, JSON: true})
	var rep DiffReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil || code != DiffExitOutside {
		t.Fatalf("json exit %d err %v\n%s", code, err, jsonOut)
	}
	checkJSONMatchesText(t, rep)
}

func checkJSONMatchesText(t *testing.T, rep DiffReport) {
	t.Helper()
	var paths []string
	for _, f := range rep.Files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"other/extra.md", "other/o r.txt", "pkg/new_test.go", "pkg/p.go"}) {
		t.Errorf("json paths = %v", paths)
	}
	if !slices.Equal(rep.Outside, []string{"other/extra.md", "other/o r.txt"}) || rep.Files[1].From != "other/o.txt" {
		t.Errorf("json outside = %v, rename from %q", rep.Outside, rep.Files[1].From)
	}
	if tot := rep.Totals; tot.Added != 8 || tot.Deleted != 1 || tot.Test.Added != 3 || tot.Prod.Added != 5 {
		t.Errorf("json totals = %+v", tot)
	}
}

func TestDiffBaseAndCleanTree(t *testing.T) {
	dir := newDiffRepo(t)
	if code, out, _ := runDiff(t, DiffOptions{Dir: dir}); code != 0 || out != "agents diff: no changes\n" {
		t.Errorf("clean tree: exit %d out %q", code, out)
	}
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n")
	repoGit(t, dir, "commit", "-q", "-am", "second")
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Base: "HEAD~1"})
	if code != 0 || !strings.Contains(squeeze(out), "M +0 -2 pkg/p.go\n") {
		t.Errorf("--base HEAD~1: exit %d\n%s", code, out)
	}
	if code, _, errOut := runDiff(t, DiffOptions{Dir: dir, Base: "--output=x"}); code != DiffExitUsage || !strings.Contains(errOut, "must not start") {
		t.Errorf("flag-shaped base: exit %d stderr %q", code, errOut)
	}
}

func TestDiffRejectsBadPath(t *testing.T) {
	dir := newDiffRepo(t)
	for path, msg := range map[string]string{"../x": "inside the repo root", "pkg/../../x": "inside the repo root",
		"/etc": "inside the repo root", t.TempDir(): "inside the repo root", "a,b": "repeat --path"} {
		code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Paths: []string{path}})
		if code != DiffExitUsage || out != "" || !strings.Contains(errOut, msg) {
			t.Errorf("--path %q: exit %d stdout %q stderr %q, want 2 and %q", path, code, out, errOut, msg)
		}
	}
	got, err := cleanPaths(dir, []string{filepath.Join(dir, "pkg"), "./pkg/p.go", "rules/**/*.md"})
	if err != nil || !slices.Equal(got, []string{"pkg", "pkg/p.go", "rules/**/*.md"}) {
		t.Errorf("cleanPaths = %v, %v", got, err)
	}
}

func TestInPathsPrefixAndGlob(t *testing.T) {
	sel := []string{"internal/memory", "rules/**/*.md", "internal/cli/memory.go"}
	for p, want := range map[string]bool{
		"internal/memory/a.go": true, "internal/memory2/a.go": false, "rules/a.md": true, "rules/x/y/a.md": true,
		"rules/a.txt": false, "internal/cli/memory.go": true, "internal/cli/memory_test.go": false,
	} {
		if got := inPaths(sel, p); got != want {
			t.Errorf("inPaths(%q) = %v, want %v", p, got, want)
		}
	}
}

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
	writeRepoFile(t, cfg, "memory/home/activeContext.md", "# Focus\n\n"+long)
	writeRepoFile(t, cfg, "memory/claude-code/activeContext.md", "# Focus\n\n- short\n")
	changed := func(rel string) []DiffFile { return []DiffFile{{Path: rel, Status: "M"}} }
	dirty, clean := "memory/home/activeContext.md", "memory/claude-code/activeContext.md"

	for _, lint := range []bool{true, false} { // the memory check does not depend on mdsmith being installed
		(&fakeGates{lint: lint}).install(t)
		last := func(rel string) GateResult {
			rs := runGates(context.Background(), cfg, "HEAD", changed(rel))
			return rs[len(rs)-1]
		}
		r := last(dirty)
		if r.Name != "docs" || r.Status != "FAIL" || !strings.Contains(r.OutputHead, "home/activeContext.md") {
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

// saveVerifyFixture stores a dispatch that ran 14:49:09..14:50:09 (-05:00) and a proxy log around that window.
func saveVerifyFixture(t *testing.T, rec Record) string {
	t.Helper()
	setConfigDir(t)
	rec.Time, rec.DurationMs, rec.Status = time.Date(2026, 11, 13, 19, 49, 9, 0, time.UTC), 60_000, "ok"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	log := strings.Join([]string{
		"proxy: listening on 127.0.0.1:1",
		"2026-11-13T14:49:50Z prod m1 200 1 1s",
		"2026-11-13T14:48:00-05:00 prod m1 200 1 1s",
		"2026-11-13T14:49:10-05:00 prod m1 200 1 1s via=h1 req=r1",
		"2026-11-13T14:49:20-05:00 prod m1 200 1 1s via=h1 stream=broken",
		"2026-11-13T14:49:30-05:00 prod m1 429 1 1s",
		"2026-11-13T14:49:45-05:00 dev m1 200 1 1s",
		"2026-11-13T14:49:46-05:00 prod m2 200 1 1s",
		"2026-11-13T19:49:50Z prod m1 200 1 1s",
		"2026-11-13T14:51:00-05:00 prod m1 200 1 1s",
	}, "\n") + "\n"
	p := filepath.Join(paths.StateDir(), "proxy.log")
	writeRepoFile(t, filepath.Dir(p), filepath.Base(p), log)
	return p
}

func wantInOutput(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("output lacks %q:\n%s", w, text)
		}
	}
}

// The window keeps 4 of the 9 lines: the Z-form line counts, the same instant read as 14:49:50Z does not.
func TestVerifyCountsProxyLinesInTheWindow(t *testing.T) {
	logPath := saveVerifyFixture(t, Record{ID: "d1", Agent: "codex", Provider: "prod", Model: "m1", Route: "databricks:prod"})
	v, err := verifyDispatch("d1", logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := v.Proxy
	got.Note = ""
	want := ProxyMatch{Match: "time window", Requests: 4, Remote: 2, Hosts: []string{"h1"}, Broken: 1, NonOK: map[string]int{"429": 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proxy = %+v, want %+v", got, want)
	}
	var out strings.Builder
	renderVerify(&out, v)
	wantInOutput(t, out.String(), "ran on: codex, workspace prod (databricks:prod), model m1", "fallback: none",
		"no proxy log line carries dispatch=d1", "fell back to the time window", "may include other traffic", "via remote host: 2 (h1)",
		"stream=broken: 1", "non-200: 429 x1")
	if v.Mismatch || strings.Contains(out.String(), "MISMATCH") {
		t.Errorf("a run as requested must not mismatch: %v", v.Mismatches)
	}
}

// A dispatch= tag selects its own lines exactly, even from a concurrent dispatch on the same workspace and model.
func TestVerifyMatchesByDispatchTag(t *testing.T) {
	logPath := saveVerifyFixture(t, Record{ID: "d3", Agent: "codex", Provider: "prod", Model: "m1"})
	lines := []string{
		"2026-11-13T14:49:10-05:00 prod m1 200 1 1s via=h1 dispatch=d3",
		"2026-11-13T14:49:20-05:00 prod m1 200 1 1s dispatch=d3 stream=broken:reset",
		"2026-11-13T14:49:25-05:00 prod m1 429 1 1s dispatch=other",
		"2026-11-13T14:49:30-05:00 prod m1 200 1 1s",
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := verifyDispatch("d3", logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := v.Proxy
	got.Note = ""
	want := ProxyMatch{Match: "dispatch id", Requests: 2, Remote: 1, Hosts: []string{"h1"}, Broken: 1, NonOK: map[string]int{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proxy = %+v, want %+v", got, want)
	}
}

// With no tag for this dispatch the window fallback counts untagged lines only, never another dispatch's.
func TestVerifyFallbackSkipsOtherDispatchLines(t *testing.T) {
	logPath := saveVerifyFixture(t, Record{ID: "d4", Agent: "codex", Provider: "prod", Model: "m1"})
	lines := []string{
		"2026-11-13T14:49:25-05:00 prod m1 429 1 1s dispatch=other",
		"2026-11-13T14:49:30-05:00 prod m1 200 1 1s",
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := verifyDispatch("d4", logPath)
	if err != nil {
		t.Fatal(err)
	}
	if p := v.Proxy; p.Match != "time window" || p.Requests != 1 || len(p.NonOK) != 0 {
		t.Errorf("proxy = %+v, want the one untagged line by time window", p)
	}
}

// parseProxyLine reads the dispatch token and both stream=broken forms.
func TestParseProxyLineTokens(t *testing.T) {
	for line, want := range map[string]proxyLine{
		"2026-11-13T14:49:20-05:00 p m 200 1 1s via=h dispatch=d1 stream=broken":   {via: "h", dispatch: "d1", broken: true},
		"2026-11-13T14:49:20-05:00 p m 200 1 1s dispatch=d1 stream=broken:timeout": {dispatch: "d1", broken: true},
		"2026-11-13T14:49:20-05:00 p m 200 1 1s stream=brokenish":                  {},
	} {
		got, ok := parseProxyLine(line)
		got.ts, got.profile, got.model, got.status = time.Time{}, "", "", ""
		if !ok || got != want {
			t.Errorf("parseProxyLine(%q) = %+v, %t; want %+v", line, got, ok, want)
		}
	}
}

// Finding 4: an upstream error message in msg= cannot forge dispatch= or stream=broken; metadata ends at msg=.
func TestParseProxyLineStopsAtMsg(t *testing.T) {
	line := `2026-11-13T14:49:20-05:00 p m 502 1 1s dispatch=d1 err=x msg="boom dispatch=evil stream=broken via=h req=r"`
	got, ok := parseProxyLine(line)
	want := proxyLine{dispatch: "d1"}
	got.ts, got.profile, got.model, got.status = time.Time{}, "", "", ""
	if !ok || got != want {
		t.Errorf("parseProxyLine = %+v, %t; want %+v", got, ok, want)
	}
}

// Finding 8: a slow old-stamped line appended last must not hide an earlier in-window line; a hit byte bound is noted.
func TestVerifyScanSurvivesOldStampedLateLine(t *testing.T) {
	logPath := saveVerifyFixture(t, Record{ID: "d8", Agent: "codex", Provider: "prod", Model: "m1"})
	lines := []string{
		"2026-11-13T14:49:10-05:00 prod m1 200 1 1s dispatch=d8",
		"2026-11-13T14:20:00-05:00 prod m1 200 1 29m50s",
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := verifyDispatch("d8", logPath)
	if err != nil {
		t.Fatal(err)
	}
	if p := v.Proxy; p.Match != "dispatch id" || p.Requests != 1 || strings.Contains(p.Note, "stopped") {
		t.Fatalf("proxy = %+v, want the tagged line found, no bound note", p)
	}
	defer func(n int64) { verifyScanCap = n }(verifyScanCap)
	verifyScanCap = 1
	v, _ = verifyDispatch("d8", logPath)
	if !strings.Contains(v.Proxy.Note, "scan stopped") {
		t.Errorf("note %q does not say the scan bound was hit", v.Proxy.Note)
	}
}

func TestVerifyMismatchExitsOneAndUnknownIDExitsTwo(t *testing.T) {
	saveVerifyFixture(t, Record{ID: "d2", Agent: "codex", Provider: "prod", Model: "m1", Fallback: "claude",
		FallbackReason: "429", Request: RunOptions{Model: "other", Provider: "qa"}})
	dir := newDiffRepo(t)
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "d2"})
	if code != DiffExitGate {
		t.Errorf("exit %d, want 1", code)
	}
	wantInOutput(t, out, "MISMATCH fallback: claude -> codex (429)", "MISMATCH workspace:", "MISMATCH model:")
	code, jsonOut, _ := runDiff(t, DiffOptions{Dir: dir, Verify: "d2", JSON: true})
	var rep DiffReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil || code != DiffExitGate || rep.Verify == nil || !rep.Verify.Mismatch {
		t.Errorf("json exit %d err %v verify %+v", code, err, rep.Verify)
	}
	code, out, errOut := runDiff(t, DiffOptions{Dir: dir, Verify: "nope"})
	if code != DiffExitUsage || out != "" {
		t.Errorf("unknown id: exit %d stdout %q, want 2 and none", code, out)
	}
	wantInOutput(t, errOut, `--verify "nope"`)
}
