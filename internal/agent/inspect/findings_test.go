package inspect

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func rowStrings(rows []Finding) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.String()
	}
	return out
}

// The samples are real tool output captured on a scratch module; chrome lines must not become rows.
func TestParsersTurnToolOutputIntoRows(t *testing.T) {
	cases := []struct {
		name  string
		parse parseFn
		out   string
		want  []string
	}{
		{"gofmt", parseGofmt, "p/a.go\np/c.go:2:7: expected ')', found 'EOF'\n",
			[]string{"p/a.go: gofmt: needs formatting", "p/c.go:2: gofmt: expected ')', found 'EOF'"}},
		{"vet", parseGoOutput("vet"), "# x/p\nvet: p/a.go:4:2: declared and not used: x\np/c.go:5:24: fmt.Printf format %d has arg \"s\"\n",
			[]string{"p/a.go:4: vet: declared and not used: x", "p/c.go:5: vet: fmt.Printf format %d has arg \"s\""}},
		{"golangci", parseGolangci, "p/c.go:5:24: printf: fmt.Printf format %d has arg \"s\" (govet)\nfunc H() { fmt.Printf(\"%d\", \"s\") }\n" +
			"                       ^\np/a.go:6:2: ineffectual assignment to x (ineffassign)\n\tx = 2\n\t^\n2 issues:\n* govet: 1\n* ineffassign: 1\n",
			[]string{"p/a.go:6: golangci-lint/ineffassign: ineffectual assignment to x",
				"p/c.go:5: golangci-lint/govet: printf: fmt.Printf format %d has arg \"s\""}},
		{"golangci keeps an unparsed line", parseGolangci, "p/a.go:1: : # x/p\n", []string{"golangci-lint: p/a.go:1: : # x/p"}},
		{"mdsmith", parseDocs, "a.md:3:1 MDS008 multiple consecutive blank lines\n1 | # T\n2 | \n3 | \n····^\n4 | \n" +
			"stats: checked=1 fixed=0 failures=1 unfixed=1\n", []string{"a.md:3: mdsmith/MDS008: multiple consecutive blank lines"}},
		{"yamlfmt", parseYamlfmt, "The following formatting differences were found:\n\nc.yml:\n- a:   1    a: 1\n            \n\n",
			[]string{"c.yml: yamlfmt: needs formatting"}},
		{"shellcheck", parseShellcheck, "s.sh:3:4: error: Couldn't parse this test expression. [SC1073]\n",
			[]string{"s.sh:3: shellcheck/SC1073: Couldn't parse this test expression."}},
		{"quality", parseQuality, "pkg/p.go:12: comment over 120 chars\n1 new violation(s) vs abc (rules/standards/quality.md)\n",
			[]string{"pkg/p.go:12: quality: comment over 120 chars"}},
		{"test", parseTest, "--- FAIL: TestX (0.00s)\n    p_test.go:9: got 1\nFAIL\nFAIL\tx/p\t0.2s\n",
			[]string{"test: --- FAIL: TestX (0.00s)", "p_test.go:9: test: got 1", "x/p: test: FAIL"}},
		{"gofmt spaced", parseGofmt, "internal/a b/y.go\ninternal/ab/bad file.go:2:8: expected ')', found '{'\n",
			[]string{"internal/a b/y.go: gofmt: needs formatting", "internal/ab/bad file.go:2: gofmt: expected ')', found '{'"}},
		{"vet spaced", parseGoOutput("vet"), "# x/internal/ab\nvet: internal/ab/bad file.go:2:8: expected ')', found '{'\n",
			[]string{"internal/ab/bad file.go:2: vet: expected ')', found '{'"}},
		{"vet windows", parseGoOutput("vet"), "C:\\x\\y.go:12:3: bad: a: b\n", []string{"C:\\x\\y.go:12: vet: bad: a: b"}},
		{"golangci spaced", parseGolangci, "internal/ab/p q.go:7:2: declared and not used: x (typecheck)\npackage ab\n1 issues:\n* typecheck: 1\n",
			[]string{"internal/ab/p q.go:7: golangci-lint/typecheck: declared and not used: x"}},
		{"mdsmith spaced", parseDocs, "docs/my notes.md:3:1 MDS008 multiple consecutive blank lines\n1 | # T\n····^\n" +
			"docs/my notes.md: memory fmt --check: would change\n",
			[]string{"docs/my notes.md: memory-fmt: would change", "docs/my notes.md:3: mdsmith/MDS008: multiple consecutive blank lines"}},
		{"yamlfmt spaced", parseYamlfmt, "The following formatting differences were found:\n\ndocs/c d.yml:\n- a:   1  a: 1\n", []string{"docs/c d.yml: yamlfmt: needs formatting"}},
		{"shellcheck spaced", parseShellcheck, "scripts/my script.sh:2:4: error: Couldn't parse: a: b. [SC1073]\n",
			[]string{"scripts/my script.sh:2: shellcheck/SC1073: Couldn't parse: a: b."}},
		{"quality spaced", parseQuality, "docs/my notes.go:12: comment: over 120 chars\n", []string{"docs/my notes.go:12: quality: comment: over 120 chars"}},
		{"test spaced", parseTest, "    my file_test.go:9: got 1: want 2\n", []string{"my file_test.go:9: test: got 1: want 2"}},
	}
	for _, c := range cases {
		if got := rowStrings(c.parse(c.out)); !slices.Equal(got, c.want) {
			t.Errorf("%s rows =\n%q\nwant\n%q", c.name, got, c.want)
		}
	}
}

// scopeRepo commits pkg/p.go and pkg/q.go, then dirties only pkg/p.go; the fake records every gate command.
func scopeRepo(t *testing.T) (dir string, calls *[][]string) {
	t.Helper()
	dir = newDiffRepo(t)
	writeRepoFile(t, dir, "pkg/q.go", "package pkg\n\nfunc Q() int { return 2 }\n")
	repoGit(t, dir, "add", "-A")
	repoGit(t, dir, "commit", "-q", "-m", "q")
	writeRepoFile(t, dir, "pkg/p.go", "package pkg\n\nfunc P() int { return 3 }\n")
	(&fakeGates{lint: true}).install(t)
	rec := &[][]string{}
	gateRun = func(_ context.Context, _, name string, args ...string) (string, error) {
		*rec = append(*rec, append([]string{name}, args...))
		return "", nil
	}
	return dir, rec
}

func callOf(calls [][]string, name string) []string {
	for _, c := range calls {
		if strings.HasSuffix(c[0], name) {
			return c[1:]
		}
	}
	return nil
}

func TestCheckScopeDirtyAllAndPath(t *testing.T) {
	dir, calls := scopeRepo(t)
	run := func(o DiffOptions, wantCode int) {
		*calls = nil
		o.Dir, o.Check, o.Gates = dir, true, []string{"gofmt,vet"}
		if code, out, _ := runDiff(t, o); code != wantCode {
			t.Fatalf("exit %d, want %d: %s", code, wantCode, out)
		}
	}
	run(DiffOptions{}, 0)
	if got := callOf(*calls, "gofmt"); !slices.Equal(got, []string{"-l", "pkg/p.go"}) {
		t.Errorf("dirty scope gofmt args = %v, want only the dirty file", got)
	}
	run(DiffOptions{All: true}, 0)
	if got := callOf(*calls, "gofmt"); !slices.Equal(got, []string{"-l", "pkg/p.go", "pkg/p_test.go", "pkg/q.go"}) {
		t.Errorf("--all gofmt args = %v, want every Go file", got)
	}
	if got := callOf(*calls, "go"); !slices.Equal(got, []string{"vet", "./..."}) {
		t.Errorf("--all vet args = %v, want ./...", got)
	}
	run(DiffOptions{All: true, Paths: []string{"pkg/q.go"}}, DiffExitOutside) // the dirty p.go is outside --path
	if got := callOf(*calls, "gofmt"); !slices.Equal(got, []string{"-l", "pkg/q.go"}) {
		t.Errorf("--all --path gofmt args = %v, want only the named file", got)
	}
	if got := callOf(*calls, "go"); !slices.Equal(got, []string{"vet", "./pkg"}) {
		t.Errorf("--all --path vet args = %v, want ./pkg", got)
	}
}

func TestGatesFlagPicksGatesAndRejectsUnknown(t *testing.T) {
	dir, calls := scopeRepo(t)
	if code, out, _ := runDiff(t, DiffOptions{Dir: dir, Check: true, Gates: []string{"lint"}}); code != 0 || strings.Contains(out, "build") {
		t.Errorf("--gates lint: exit %d, out %q (lint never builds or tests)", code, out)
	}
	if c := callOf(*calls, "test"); c != nil || len(*calls) == 0 {
		t.Errorf("--gates lint ran %v", *calls)
	}
	if code, _, errOut := runDiff(t, DiffOptions{Dir: dir, Check: true, Gates: []string{"nope"}}); code != DiffExitUsage || !strings.Contains(errOut, "nope") {
		t.Errorf("unknown gate: exit %d, stderr %q; want usage", code, errOut)
	}
	if code, _, _ := runDiff(t, DiffOptions{Dir: dir, All: true}); code != DiffExitUsage {
		t.Errorf("--all without --check: exit %d, want usage", code)
	}
}

// A missing tool fails with one row when --gates was given, and keeps skipping on the default gate set.
func TestMissingToolFailsOnlyWithGates(t *testing.T) {
	dir, _ := scopeRepo(t)
	gateLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Check: true, Gates: []string{"golangci-lint"}})
	if want := "golangci-lint: golangci-lint not installed (make install DEV_TOOLS=1)"; code != DiffExitGate || !strings.Contains(out, want) {
		t.Errorf("--gates: exit %d, out %q; want exit 1 and the row %q", code, out, want)
	}
	if code, out, _ := runDiff(t, DiffOptions{Dir: dir, Check: true}); code != 0 || !strings.Contains(out, "skip golangci-lint (golangci-lint not installed)") {
		t.Errorf("default: exit %d, out %q; want the skip as before", code, out)
	}
}

func TestQualityGateRunsSimplifyFailAndReadsRows(t *testing.T) {
	dir, _ := scopeRepo(t)
	var args []string
	gateSimplify = func(a []string, stdout, _ io.Writer) int {
		args = a
		_, _ = io.WriteString(stdout, "pkg/p.go:3: comment is 130 chars\n1 new violation(s) vs HEAD (rules/standards/quality.md)\n")
		return 1
	}
	code, out, _ := runDiff(t, DiffOptions{Dir: dir, Check: true, Gates: []string{"quality"}, JSON: true})
	var rep DiffReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != DiffExitGate {
		t.Fatalf("exit %d, json err %v: %s", code, err, out)
	}
	if f := rep.Gates[0].Findings; len(f) != 1 || f[0].Path != "pkg/p.go" || f[0].Line != 3 || f[0].Tool != "quality" {
		t.Errorf("findings = %+v, want the one parsed row as an object", f)
	}
	if len(args) < 2 || args[0] != "--fail" || args[len(args)-1] != "pkg/p.go" && !strings.HasSuffix(args[len(args)-1], "pkg/p.go") {
		t.Errorf("simplify args = %v, want --fail and the dirty file", args)
	}
	runDiff(t, DiffOptions{Dir: dir, Check: true, All: true, Gates: []string{"quality"}})
	if !slices.Equal(args, []string{"--fail"}) {
		t.Errorf("--all simplify args = %v, want bare --fail as make runs it", args)
	}
}

// Whole-tree docs lets mdsmith's own config pick the files; an explicit file list would bypass its ignore list.
func TestAllDocsLetsMdsmithPickItsFiles(t *testing.T) {
	dir, calls := scopeRepo(t)
	writeRepoFile(t, dir, "notes.md", "# N\n")
	if code, out, _ := runDiff(t, DiffOptions{Dir: dir, Check: true, All: true, Gates: []string{"docs"}}); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if got := callOf(*calls, "mdsmith"); !slices.Equal(got, []string{"check", "--no-color"}) {
		t.Errorf("--all mdsmith args = %v, want no file list", got)
	}
	*calls = nil
	runDiff(t, DiffOptions{Dir: dir, Check: true, Gates: []string{"docs"}})
	if got := callOf(*calls, "mdsmith"); !slices.Equal(got, []string{"check", "--no-color", "--", "notes.md"}) {
		t.Errorf("dirty mdsmith args = %v, want the changed file after --", got)
	}
}
