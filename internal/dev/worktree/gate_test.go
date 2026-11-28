package worktree

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	verifyinspect "github.com/edotau/claude-code/internal/agent/inspect"
	"github.com/edotau/claude-code/internal/platform/hosttest"
)

func TestReuseGoGateOnlySkipsMatchingTests(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	hosttest.WriteRel(t, dir, "go.mod", "module x\n")
	hosttest.WriteRel(t, dir, "pkg/p.go", "package pkg\nconst N = 1\n")
	key, err := verifyinspect.ComputeStampKey(dir, []string{"pkg"}, verifyinspect.GoStampGates)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyinspect.WriteStamp(key, verifyinspect.StampMeta{Passed: true, Gates: verifyinspect.GoStampGates}); err != nil {
		t.Fatal(err)
	}
	hosttest.WriteRel(t, dir, "unrelated/broken.go", "invalid Go\n")
	steps := []string{"go build ./...", "go vet ./...", "go test ./pkg"}
	var out bytes.Buffer
	left := uncachedGoSteps(dir, steps, &out, map[string]float64{})
	if !slices.Equal(left, steps[:2]) || !strings.Contains(out.String(), "stamp-reused") {
		t.Fatalf("narrow proof waived wider checks: %v %s", left, out.String())
	}
	if reuseGoGate(dir, [][]string{{"go test -race ./pkg"}}) {
		t.Fatal("custom test flags were suppressed")
	}
	hosttest.WriteRel(t, dir, "pkg/p.go", "package pkg\nconst N = 2\n")
	if reuseGoGate(dir, [][]string{{"go test ./pkg"}}) {
		t.Fatal("modified package reused stale pass")
	}
}

// gateRepo is a git repo holding module example.com/m, with run and write helpers.
func gateRepo(t *testing.T) (dir string, run func(...string) string, write func(rel, body string)) {
	t.Helper()
	dir = gitRepo(t)
	run = func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write = func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/m\n\ngo 1.22\n")
	return dir, run, write
}

// TestScopedGoGate covers package selection, registry packages, non-Go and deleted paths, and the go.mod fallback.
func TestScopedGoGate(t *testing.T) {
	dir, run, write := gateRepo(t)
	write("internal/old/old.go", "package old\n")
	write("internal/hooks/hooks.go", "package hooks\n")
	run("add", "-A")
	run("commit", "-q", "-m", "old")
	base := run("rev-parse", "HEAD")

	write("internal/foo/foo.go", "package foo\n")
	write("internal/foo/testdata/in.txt", "x\n")
	write("docs/readme.md", "x\n")
	if err := os.Remove(filepath.Join(dir, "internal/old/old.go")); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "change")

	cmd, pkgs, _, sc, err := scopedGoGate(context.Background(), dir, base)
	if err != nil || !sc.Scoped {
		t.Fatalf("err=%v scoped=%v", err, sc.Scoped)
	}
	if want := []string{"internal/foo"}; !slices.Equal(pkgs, want) {
		t.Errorf("pkgs=%v want %v", pkgs, want)
	}
	if cmd != "go build ./... && go vet ./... && go test ./internal/foo" {
		t.Errorf("cmd=%s", cmd)
	}

	write("go.mod", "module example.com/m\n\ngo 1.23\n")
	run("add", "-A")
	run("commit", "-q", "-m", "mod")
	cmd, _, _, sc, err = scopedGoGate(context.Background(), dir, base)
	if err != nil || sc.Scoped || !strings.HasSuffix(cmd, "go test ./...") {
		t.Fatalf("go.mod change: cmd=%s scoped=%v err=%v", cmd, sc.Scoped, err)
	}
}

// TestScopedGoGateReverseDeps: a changed package and its direct importers run; transitive importers do not.
func TestScopedGoGateReverseDeps(t *testing.T) {
	dir, run, write := gateRepo(t)
	write("a/a.go", "package a\n")
	write("b/b.go", "package b\n\nimport _ \"example.com/m/a\"\n")
	write("c/c.go", "package c\n\nimport _ \"example.com/m/b\"\n")
	write("d/d.go", "package d\n")
	write("d/d_test.go", "package d_test\n\nimport _ \"example.com/m/c\"\n")
	write("e/e.go", "package e\n\nimport _ \"example.com/m/d\"\n")
	write("f/f.go", "package f\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	write("a/a.go", "package a\n\nconst X = 1\n")
	run("commit", "-q", "-am", "change a")

	_, pkgs, nChanged, _, err := scopedGoGate(context.Background(), dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b"}; !slices.Equal(pkgs, want) {
		t.Errorf("pkgs=%v want %v", pkgs, want)
	}
	if nChanged != 1 {
		t.Errorf("nChanged=%d want 1", nChanged)
	}
}

// Regression: goldens, embeds and unowned non-markdown files select their consumers; unowned markdown selects nothing.
func TestScopedGoGateNonGoFiles(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	dir, run, write := gateRepo(t)
	write("mem/mem.go", "package mem\n")
	write("mem/mem_test.go", "package mem\n\nconst golden = \"../docs/examples/mem\"\n")
	write("emb/emb.go", "package emb\n\nimport _ \"embed\"\n\n//go:embed sub/x.json\nvar x string\n")
	write("emb/sub/sub.go", "package sub\n")
	write("emb/sub/x.json", "{}\n")
	write("z/z.go", "package z\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	write("docs/examples/mem/index.md", "x\n")
	write("emb/sub/x.json", "{\"a\":1}\n")
	write("docs/notes.md", "x\n")
	run("add", "-A")
	run("commit", "-q", "-m", "data")

	_, pkgs, _, sc, err := scopedGoGate(context.Background(), dir, base)
	if err != nil || !sc.Scoped {
		t.Fatalf("err=%v scoped=%v", err, sc.Scoped)
	}
	if want := []string{"emb", "emb/sub", "mem"}; !slices.Equal(pkgs, want) {
		t.Errorf("pkgs=%v want %v", pkgs, want)
	}

	write("tools/run.sh", "#!/bin/sh\n")
	run("add", "-A")
	run("commit", "-q", "-m", "script")
	if cmd, _, _, sc, err := scopedGoGate(context.Background(), dir, base); err != nil || sc.Scoped || cmd != fullGoGate {
		t.Fatalf("unowned non-markdown file: cmd=%s scoped=%v err=%v", cmd, sc.Scoped, err)
	}
}

// TestScopedVerifyGate covers scope rendering per toolchain and that a Makefile test target is never the gate.
func TestScopedVerifyGate(t *testing.T) {
	const goBase = fullGoGate
	const lintVerbBase = "claude-code agents diff --check --gates lint"
	const lintAll = lintVerbBase + " --all"
	cases := []struct {
		name, files string
		scope       []string
		lint        bool
		gate, src   string
	}{
		{"go empty scope", "go.mod", nil, false, goBase, "go.mod"},
		{"go root scope", "go.mod", []string{"./"}, false, goBase, "go.mod"},
		{"go dot scope", "go.mod", []string{"."}, false, goBase, "go.mod"},
		{"go one dir", "go.mod", []string{"./internal/x/"}, false,
			"go build ./... && go vet './internal/x/...' && go test './internal/x/...'", "go.mod"},
		{"go multi dir", "go.mod", []string{"a", "./b"}, false,
			"go build ./... && go vet './a/...' './b/...' && go test './a/...' './b/...'", "go.mod"},
		{"no toolchain", "", nil, false, "", "none"},
		{"makefile test only is not a gate", "Makefile", nil, false, "", "none"},
		{"go wins over makefile test", "go.mod,Makefile", nil, false, goBase, "go.mod"},
		{"lint-check prefixes the verb", "go.mod,Makefile:lint", nil, true, lintAll + " && " + goBase, "go.mod"},
		{"lint-check scopes the verb", "go.mod,Makefile:lint", []string{"./a/", "b c"}, true,
			lintVerbBase + ` --base "${HARNESS_LAND_BASE:-HEAD}" --path a --path 'b c'` + " && go build ./... && go vet './a/...' './b c/...' && go test './a/...' './b c/...'", "go.mod"},
		{"lint-only without toolchain", "Makefile:lint", nil, true, "", "none"},
		{"lint-check with makefile test only", "go.mod,Makefile", nil, true, goBase, "go.mod"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGateFiles(t, dir, c.files)
			gate, src := ScopedVerifyGate(dir, c.scope, c.lint)
			if gate != c.gate || src != c.src {
				t.Errorf("gate=%q source=%q, want %q %q", gate, src, c.gate, c.src)
			}
		})
	}
}

// writeGateFiles writes toolchain markers, a CHECK-aware lint recipe, or a test-only Makefile.
func writeGateFiles(t *testing.T, dir, spec string) {
	t.Helper()
	for entry := range strings.SplitSeq(spec, ",") {
		if entry == "" {
			continue
		}
		body := "module m\n"
		if entry == "Makefile" {
			body = "test:\n\ttrue\n"
		}
		if entry == "Makefile:lint" {
			entry, body = "Makefile", "lint:\n\tgo run ./cmd/lint --check=$(CHECK)\n"
		}
		if err := os.WriteFile(filepath.Join(dir, entry), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A scoped lint compares against the merge's pinned target (the lane is committed); HEAD when no merge runs.
func TestScopedLintDiffsAgainstMergeBase(t *testing.T) {
	dir := t.TempDir()
	writeGateFiles(t, dir, "go.mod,Makefile:lint")
	gate, _ := ScopedVerifyGate(dir, []string{"pkg"}, true)
	lint, _, _ := strings.Cut(gate, " && ")
	stub := "claude-code() { printf '<%s>' \"$@\"; }; "
	for base, want := range map[string]string{"abc123": "<--base><abc123>", "": "<--base><HEAD>"} {
		var out strings.Builder
		if err := runVerify(context.Background(), dir, stub+lint, base, &out, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), want) {
			t.Errorf("base %q: lint args %q lack %q", base, out.String(), want)
		}
	}
}

func TestScopedVerifyGateQuotesScope(t *testing.T) {
	scope := []string{"tests/my pkg", "dir[1]", "x; echo injected", "$(printf substituted)"}
	goTargets := " <./tests/my pkg/...> <./dir[1]/...> <./x; echo injected/...> <./$(printf substituted)/...>\n"
	pyTargets := " <tests/my pkg> <dir[1]> <x; echo injected> <$(printf substituted)>\n"
	stub := "go() { printf go; printf ' <%s>' \"$@\"; printf '\\n'; }; " +
		"pytest() { printf pytest; printf ' <%s>' \"$@\"; printf '\\n'; }; " +
		"ruff() { printf ruff; printf ' <%s>' \"$@\"; printf '\\n'; }; "
	for _, c := range []struct {
		name, files, want string
		scope             []string
	}{
		{"go", "go.mod", "go <build> <./...>\ngo <vet>" + goTargets + "go <test>" + goTargets, scope},
		{"python", "pyproject.toml,ruff.toml", "ruff <check>" + pyTargets + "pytest" + pyTargets, scope},
		{"go apostrophe", "go.mod", "go <build> <./...>\ngo <vet> <./quote's/...>\ngo <test> <./quote's/...>\n", []string{"quote's"}},
		{"python apostrophe", "pyproject.toml,ruff.toml", "ruff <check> <quote's>\npytest <quote's>\n", []string{"quote's"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGateFiles(t, dir, c.files)
			if err := os.MkdirAll(filepath.Join(dir, "dir1", "..."), 0o755); err != nil {
				t.Fatal(err)
			}
			gate, _ := ScopedVerifyGate(dir, c.scope, false)
			var out, stderr strings.Builder
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := runVerify(ctx, dir, stub+gate, "", &out, &stderr); err != nil {
				t.Fatalf("runVerify: %v; stderr=%s", err, &stderr)
			}
			if got := out.String(); got != c.want {
				t.Fatalf("scope arguments changed during shell execution:\ngot  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestScopedVerifyGateRuffTable(t *testing.T) {
	for _, c := range []struct {
		name, toml string
		wantRuff   bool
	}{
		{"no table", "[project]\nname = 'example'\n", false},
		{"comment", "# [tool.ruff]\n", false},
		{"other table", "[tool.ruffle]\n", false},
		{"string value", "description = '[tool.ruff]'\n", false},
		{"ruff table", "[tool.ruff]\n", true},
		{"ruff subtable", "[tool.ruff.lint]\n", true},
		{"indented table", "  [tool.ruff] # lint configuration\n", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(c.toml), 0o644); err != nil {
				t.Fatal(err)
			}
			want := "(pytest)"
			if c.wantRuff {
				want = "ruff check . && " + want
			}
			if gate, source := ScopedVerifyGate(dir, nil, false); gate != want || source != "pyproject" {
				t.Fatalf("gate=%q source=%q, want %q from pyproject", gate, source, want)
			}
		})
	}
}

// A cancelled ctx must kill the long-running gate promptly instead of letting it finish.
func TestRunVerifyCancelledByContextStopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runVerify(ctx, t.TempDir(), "sleep 30", "", io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runVerify = %v, want context.Canceled without running the gate", err)
	}
}

// Precedence: --no-verify, then --verify, then detection (Makefile test target beats go.mod); nothing found refuses.
func TestChooseVerifyGatePrecedence(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := chooseVerifyGate(dir, "", false); err == nil {
		t.Fatal("empty dir without --no-verify: want a refusal")
	}
	if g, _, err := chooseVerifyGate(dir, "x", true); g != "" || err != nil {
		t.Fatalf("--no-verify = (%q, %v), want skip", g, err)
	}
	if g, s, _ := chooseVerifyGate(dir, "x", false); g != "x" || s != "flag" {
		t.Fatalf("--verify = (%q, %q)", g, s)
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o644)
	if _, s, _ := chooseVerifyGate(dir, "", false); s != "go.mod" {
		t.Fatalf("go.mod source = %q", s)
	}
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\ttrue\n"), 0o644)
	if g, s, _ := chooseVerifyGate(dir, "", false); g != "make test" || s != "Makefile" {
		t.Fatalf("Makefile = (%q, %q)", g, s)
	}
}

// Regression: a markdown-only change selects no packages and must not emit a bare `go test`.
func TestScopedGoGateNoPackagesOmitsGoTest(t *testing.T) {
	dir, run, write := gateRepo(t)
	write("a/a.go", "package a\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	write("docs/readme.md", "x\n")
	run("add", "-A")
	run("commit", "-q", "-m", "docs")

	cmd, pkgs, _, sc, err := scopedGoGate(context.Background(), dir, base)
	if err != nil || !sc.Scoped || len(pkgs) != 0 {
		t.Fatalf("err=%v scoped=%v pkgs=%v", err, sc.Scoped, pkgs)
	}
	if cmd != "go build ./... && go vet ./..." {
		t.Errorf("cmd=%q, want build and vet only", cmd)
	}
}

// A change in the module-root package must select it, even when the worktree path holds glob characters.
func TestScopedGoGateRootPackageAndGlobPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fix[1]")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	for name, body := range map[string]string{"go.mod": "module example.com/m\n\ngo 1.22\n", "main.go": "package main\n\nfunc main() {}\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "change")

	cmd, pkgs, _, _, err := scopedGoGate(context.Background(), dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pkgs, []string{"."}) || !strings.HasSuffix(cmd, "go test ./.") {
		t.Errorf("pkgs=%v cmd=%s; want the root package tested", pkgs, cmd)
	}
}

// Cancelling must kill the gate's grandchildren, so a non-file writer's pipe closes and Wait returns.
func TestRunVerifyCancelKillsProcessTree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	origWait, origGrace := gateWaitDelay, gateCancelGrace
	gateWaitDelay, gateCancelGrace = time.Hour, 20*time.Millisecond // only killing the group can release the pipe before this
	t.Cleanup(func() { gateWaitDelay, gateCancelGrace = origWait, origGrace })
	var out strings.Builder
	_ = runVerify(ctx, t.TempDir(), "sleep 600 & wait", "", &out, &out)
}

// The vault master key must not reach the gate process.
func TestRunVerifyDropsVaultKey(t *testing.T) {
	t.Setenv("HARNESS_VAULT_KEY", "sekrit")
	var out strings.Builder
	if err := runVerify(context.Background(), t.TempDir(), `echo "key=[${HARNESS_VAULT_KEY}]"`, "", &out, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "key=[]" {
		t.Errorf("gate saw %q, want the vault key stripped", got)
	}
}

// A failing step cancels its siblings, and every step's output still reaches the writers.
func TestRunStepsFirstFailureCancelsRest(t *testing.T) {
	origGrace := gateCancelGrace
	gateCancelGrace = 20 * time.Millisecond
	t.Cleanup(func() { gateCancelGrace = origGrace })
	var out, errOut bytes.Buffer
	start := time.Now()
	_, err := runTimed(context.Background(), gateAt{Dir: t.TempDir()}, []string{"sleep 30", "echo hi; exit 3"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "exit 3") {
		t.Fatalf("err = %v, want the failing step's exit 3", err)
	}
	if time.Since(start) > 10*time.Second || !strings.Contains(out.String(), "hi") {
		t.Fatalf("slept %v, out=%q; want the sibling cancelled and output kept", time.Since(start), out.String())
	}
}

func TestParseGateFailures(t *testing.T) {
	got := parseGateFailures("# m\n./a.go:2:5: undefined: x\n--- FAIL: TestY (0.00s)\nFAIL\n")
	if len(got) != 1 || got[0].DiagnosticID == "" {
		t.Fatalf("located failure: got %+v", got)
	}
	if got[0].DiagnosticID = ""; got[0] != (gateFailure{File: "a.go", Line: 2, Text: "undefined: x"}) {
		t.Fatalf("located failure: got %+v", got)
	}
	if got = parseGateFailures("my dir/a b.go:7: bad\n"); len(got) != 1 || got[0].File != "my dir/a b.go" || got[0].Line != 7 {
		t.Fatalf("path with spaces: got %+v", got)
	}
	got = parseGateFailures("ok\n--- FAIL: TestY (0.00s)\nFAIL\n")
	if len(got) != 1 || got[0].File != "" || got[0].Text != "--- FAIL: TestY (0.00s)" {
		t.Fatalf("unlocated failure keeps the first FAIL line: got %+v", got)
	}
}

func TestGateDiagnosticIDs(t *testing.T) {
	ids := func(out string) []string {
		var r []string
		for _, f := range parseGateFailures(out) {
			r = append(r, f.DiagnosticID)
		}
		return r
	}
	base := ids("a.go:1: bad (0.2s)\na.go:9: bad (3ms)\nb.go:1: bad\na.go:1: other\n")
	if !slices.Equal(base, ids("a.go:1: bad (0.2s)\na.go:9: bad (3ms)\nb.go:1: bad\na.go:1: other\n")) {
		t.Fatalf("ids not stable across identical input: %v", base)
	}
	if len(slices.Compact(slices.Sorted(slices.Values(base)))) != len(base) {
		t.Fatalf("ids shared within a run: %v", base)
	}
	if got := ids("a.go:40: bad (9s)\n"); got[0] != ids("a.go:7: bad (1ms)\n")[0] {
		t.Fatalf("id moved with line or timing: %v", got)
	}
	if ids("a.go:1: see /tmp/x1/a.go:40:3\n")[0] != ids("a.go:1: see /tmp/x2/a.go:50:9\n")[0] {
		t.Fatal("id moved with a temp path line")
	}
	if ids("a.go:1: bad\n")[0] == ids("b.go:1: bad\n")[0] || ids("a.go:1: bad\n")[0] == ids("a.go:1: worse\n")[0] {
		t.Fatal("distinct failures share an id")
	}
}

func TestRunScopedGateObservesExitAndChanges(t *testing.T) {
	dir, _, write := gateRepo(t)
	write("a.go", "package m\n\nfunc F() int { return x }\n")
	r, err := RunScopedGate(context.Background(), dir, nil, false)
	if err != nil || r.Exit == nil || *r.Exit == 0 {
		t.Fatalf("red gate: exit=%v err=%v, want a nonzero observed exit", r.Exit, err)
	}
	if len(r.Failures) == 0 || r.Failures[0].File != "a.go" || r.Failures[0].Line != 3 {
		t.Fatalf("failures: %+v, want a.go:3", r.Failures)
	}
	if !slices.ContainsFunc(r.Changed, func(s string) bool { return strings.HasPrefix(s, "a.go ") }) {
		t.Fatalf("changed: %v, want a.go with its hash", r.Changed)
	}
}

func TestRunScopedGateCarriesIDs(t *testing.T) {
	binDir := t.TempDir()
	stub := "#!/bin/sh\nprintf '# example.com/m\\n./a.go:3:16: undefined: x\\n' >&2\nexit 2\n"
	if err := os.WriteFile(filepath.Join(binDir, "go"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir, _, write := gateRepo(t)
	write("a.go", "package m\n\nfunc F() int { return x }\n")
	r, err := RunScopedGate(context.Background(), dir, nil, false)
	if err != nil || len(r.ChangeIDs) != len(r.Changed) || len(r.Changed) == 0 || r.ChangeIDs[0] != shortID(r.Changed[0]) {
		t.Fatalf("change ids %v do not align with changed %v (err %v)", r.ChangeIDs, r.Changed, err)
	}
	if len(r.Failures) == 0 || r.Failures[0].DiagnosticID == "" {
		t.Fatalf("failure without a diagnostic id: %+v", r.Failures)
	}
}

// The lint verb is a typed argv: a path with a space stays one word, and the base is the only expandable word.
func TestLintVerbArgv(t *testing.T) {
	got := lintVerb([]string{"a", "b c"})
	want := []string{"claude-code", "agents", "diff", "--check", "--gates", "lint", "--base", lintBase, "--path", "a", "--path", "b c"}
	if !slices.Equal(got, want) {
		t.Fatalf("lintVerb = %q, want %q", got, want)
	}
	paths := []string{lintBase, "-x", "a;b $(c)"}
	script := `claude-code() { printf '<%s>' "$@"; }; ` + lintLine(lintVerb(paths))
	out, err := exec.Command("bash", "-c", script).Output()
	wantOut := "<agents><diff><--check><--gates><lint><--base><HEAD><--path><" + lintBase + "><--path><-x><--path><a;b $(c)>"
	if err != nil || string(out) != wantOut {
		t.Fatalf("rendered line ran as %q (err %v), want %q", out, err, wantOut)
	}
}

// pytest exit 5 fails a lane that changed a .py file and passes, with a note, one that did not.
func TestEmptyPytestNeedsPyChange(t *testing.T) {
	_, run, write := gateRepo(t)
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	exit5 := exec.Command("bash", "-c", "exit 5").Run()
	write("notes.md", "x\n")
	run("add", "-A")
	run("commit", "-q", "-m", "docs")
	dir := run("rev-parse", "--show-toplevel")
	var out bytes.Buffer
	if err := emptyPytest(context.Background(), dir, base, exit5, &out); err != nil || !strings.Contains(out.String(), "no tests collected") {
		t.Fatalf("no .py change: err=%v out=%q, want pass with a note", err, out.String())
	}
	write("tests/test_x.py", "def test_a(): pass\n")
	run("add", "-A")
	run("commit", "-q", "-m", "py")
	if err := emptyPytest(context.Background(), dir, base, exit5, &out); !errors.Is(err, errNoTests) {
		t.Fatalf(".py change: err=%v, want %v", err, errNoTests)
	}
	if err := emptyPytest(context.Background(), dir, base, exec.Command("bash", "-c", "exit 1").Run(), &out); errors.Is(err, errNoTests) || err == nil {
		t.Fatalf("other exit: err=%v, want it unchanged", err)
	}
}

func TestIsPytestStep(t *testing.T) {
	for step, want := range map[string]bool{
		"(pytest)": true, "(.venv/bin/python -m pytest 'a')": true, "ruff check . && (pytest)": true,
		"check pytest.ini && make test": false, "make test": false, "(pytest) && make test": false,
	} {
		if got := isPytestStep(step); got != want {
			t.Errorf("isPytestStep(%q) = %v, want %v", step, got, want)
		}
	}
}

func TestScopedVerifyGatePythonOptionLikePath(t *testing.T) {
	for _, c := range []struct {
		name, files, want string
	}{
		{"pytest", "pyproject.toml", "(pytest './--collect-only')"},
		{"ruff and pytest", "pyproject.toml,ruff.toml", "ruff check './--collect-only' && (pytest './--collect-only')"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGateFiles(t, dir, c.files)
			gate, source := ScopedVerifyGate(dir, []string{"./--collect-only"}, false)
			if gate != c.want || source != "pyproject" {
				t.Fatalf("option-like scope must stay a path: gate=%q source=%q, want %q from pyproject", gate, source, c.want)
			}
		})
	}
}

func TestScopedVerifyGateNpmAndCargoScope(t *testing.T) {
	for _, c := range []struct {
		name, marker, body string
		scope              []string
		want, source       string
	}{
		{"npm whole suite", "package.json", `{"scripts":{"test":"vitest"}}`, nil, "npm test", "package.json"},
		{"npm scoped", "package.json", `{"scripts":{"test":"vitest"}}`, []string{"./tests/my pkg/", "other"},
			"npm test -- 'tests/my pkg' 'other'", "package.json"},
		{"cargo whole suite", "Cargo.toml", "[package]\nname = 'example'\n", nil, "cargo build && cargo test", "Cargo.toml"},
		{"cargo scoped refuses", "Cargo.toml", "[package]\nname = 'example'\n", []string{"src"}, "", "none"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.marker), []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			gate, source := ScopedVerifyGate(dir, c.scope, false)
			if gate != c.want || source != c.source {
				t.Fatalf("scope=%q: gate=%q source=%q, want %q from %q", c.scope, gate, source, c.want, c.source)
			}
		})
	}
}

// A scoped gate is [build] then [vet test]; the full go gate too, any other gate one stage.
func TestGateStages(t *testing.T) {
	stages := gateStages(scopedGateCommand([]string{"a"}), scopedGateSource+", x")
	if len(stages) != 2 || len(stages[0]) != 1 || len(stages[1]) != 2 {
		t.Fatalf("stages = %v, want [build] then [vet test]", stages)
	}
	if got := gateStages(fullGoGate, "go.mod"); len(got) != 2 || gateStages("make test", "Makefile")[0][0] != "make test" {
		t.Fatalf("full gate stages = %v", got)
	}
	if got := gateStages("a && b", "flag"); len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("--verify gate split: %v", got)
	}
}

// Stages run in order with per-step seconds recorded; a failed stage is named and later stages never run.
func TestRunStages(t *testing.T) {
	var out, errOut bytes.Buffer
	at := gateAt{Dir: t.TempDir()}
	secs, err := runStages(context.Background(), at, [][]string{{"echo one"}, {"printf two", "true"}}, &out, &errOut)
	if err != nil || len(secs) != 3 || strings.Index(out.String(), "one") > strings.Index(out.String(), "two") {
		t.Fatalf("err=%v secs=%v out=%q", err, secs, out.String())
	}
	out.Reset()
	_, err = runStages(context.Background(), at, [][]string{{"echo b; exit 2"}, {"echo test-ran"}}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "echo stage") || strings.Contains(out.String(), "test-ran") {
		t.Fatalf("err=%v out=%q, want the first stage named and no later stage run", err, out.String())
	}
}
