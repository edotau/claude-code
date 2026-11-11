package worktree

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

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

// TestScopedGoGate covers changed-package selection, present registry packages, non-Go and deleted paths, and go.mod fallback.
func TestScopedGoGate(t *testing.T) {
	dir, run, write := gateRepo(t)
	write("internal/old/old.go", "package old\n")
	write("internal/hooks/hooks.go", "package hooks\n") // one registry package present; the other four are absent and skipped
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

	cmd, pkgs, full, err := scopedGoGate(context.Background(), dir, base)
	if err != nil || full {
		t.Fatalf("err=%v full=%v", err, full)
	}
	if want := []string{"internal/foo", "internal/hooks"}; !slices.Equal(pkgs, want) {
		t.Errorf("pkgs=%v want %v", pkgs, want)
	}
	if cmd != "go build ./... && go vet ./... && go test ./internal/foo ./internal/hooks" {
		t.Errorf("cmd=%s", cmd)
	}

	write("go.mod", "module example.com/m\n\ngo 1.23\n")
	run("add", "-A")
	run("commit", "-q", "-m", "mod")
	cmd, _, full, err = scopedGoGate(context.Background(), dir, base)
	if err != nil || !full || !strings.HasSuffix(cmd, "go test ./...") {
		t.Fatalf("go.mod change: cmd=%s full=%v err=%v", cmd, full, err)
	}
}

// TestScopedGoGateReverseDeps is the regression test: importers of a changed package, transitive and via tests, run.
func TestScopedGoGateReverseDeps(t *testing.T) {
	dir, run, write := gateRepo(t)
	write("a/a.go", "package a\n")
	write("b/b.go", "package b\n\nimport _ \"example.com/m/a\"\n")           // imports a
	write("c/c.go", "package c\n\nimport _ \"example.com/m/b\"\n")           // imports a transitively
	write("d/d.go", "package d\n")                                           // only its test imports c
	write("d/d_test.go", "package d_test\n\nimport _ \"example.com/m/c\"\n") // selects d, not d's importers
	write("e/e.go", "package e\n\nimport _ \"example.com/m/d\"\n")
	write("f/f.go", "package f\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	write("a/a.go", "package a\n\nconst X = 1\n")
	run("commit", "-q", "-am", "change a")

	_, pkgs, _, err := scopedGoGate(context.Background(), dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "c", "d"}; !slices.Equal(pkgs, want) {
		t.Errorf("pkgs=%v want %v", pkgs, want)
	}
}

// TestScopedGoGateNonGoFiles is the regression test: goldens outside a package select the tests naming them, an
// embed in a sub-package dir selects its embedder, unowned markdown selects nothing, and other unowned files go full.
func TestScopedGoGateNonGoFiles(t *testing.T) {
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

	_, pkgs, full, err := scopedGoGate(context.Background(), dir, base)
	if err != nil || full {
		t.Fatalf("err=%v full=%v", err, full)
	}
	if want := []string{"emb", "emb/sub", "mem"}; !slices.Equal(pkgs, want) {
		t.Errorf("pkgs=%v want %v", pkgs, want)
	}

	write("tools/run.sh", "#!/bin/sh\n")
	run("add", "-A")
	run("commit", "-q", "-m", "script")
	if cmd, _, full, err := scopedGoGate(context.Background(), dir, base); err != nil || !full || cmd != fullGoGate {
		t.Fatalf("unowned non-markdown file: cmd=%s full=%v err=%v", cmd, full, err)
	}
}

// TestScopedVerifyGate covers scope rendering per toolchain and that a Makefile test target is never the gate.
func TestScopedVerifyGate(t *testing.T) {
	const goBase = fullGoGate
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
		{"lint-check prefixes toolchain", "go.mod,Makefile:lint", nil, true, "make lint CHECK=1 && " + goBase, "go.mod"},
		{"lint-only without toolchain", "Makefile:lint", nil, true, "make lint CHECK=1", "Makefile"},
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
		{"python", "pyproject.toml,ruff.toml", "ruff <check>" + pyTargets + "pytest <-q>" + pyTargets, scope},
		{"go apostrophe", "go.mod", "go <build> <./...>\ngo <vet> <./quote's/...>\ngo <test> <./quote's/...>\n", []string{"quote's"}},
		{"python apostrophe", "pyproject.toml,ruff.toml", "ruff <check> <quote's>\npytest <-q> <quote's>\n", []string{"quote's"}},
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
			if err := runVerify(ctx, dir, stub+gate, &out, &stderr); err != nil {
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
			want := "pytest -q"
			if c.wantRuff {
				want = "ruff check . && " + want
			}
			if gate, source := ScopedVerifyGate(dir, nil, false); gate != want || source != "pyproject" {
				t.Fatalf("gate=%q source=%q, want %q from pyproject", gate, source, want)
			}
		})
	}
}

func TestScopedVerifyGateLintRecipe(t *testing.T) {
	for _, c := range []struct {
		name, makefile string
		wantLint       bool
	}{
		{"recipe reads CHECK", "lint:\n\tif [ \"$(CHECK)\" = 1 ]; then echo check; fi\n", true},
		{"target comment", "lint: # CHECK=1 reports\n\ttrue\n", false},
		{"recipe comment", "lint:\n\ttrue # CHECK=1 reports\n", false},
		{"other recipe", "lint:\n\ttrue\n\ntest:\n\techo $(CHECK)\n", false},
		{"blank line in recipe", "lint:\n\n\techo $(CHECK)\n", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGateFiles(t, dir, "go.mod")
			if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(c.makefile), 0o644); err != nil {
				t.Fatal(err)
			}
			want := fullGoGate
			if c.wantLint {
				want = "make lint CHECK=1 && " + want
			}
			if gate, source := ScopedVerifyGate(dir, nil, true); gate != want || source != "go.mod" {
				t.Fatalf("gate=%q source=%q, want %q from go.mod", gate, source, want)
			}
		})
	}
}

// A cancelled ctx must kill the long-running gate promptly instead of letting it finish.
func TestRunVerifyCancelledByContextStopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := runVerify(ctx, t.TempDir(), "sleep 30", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("runVerify: want an error for an already-cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runVerify took %s after cancellation, want it to stop promptly", elapsed)
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

// TestScopedGoGateNoPackagesOmitsGoTest is the regression test: a markdown-only change selects no packages and must
// not emit a bare `go test`, which fails in a root dir with no Go files.
func TestScopedGoGateNoPackagesOmitsGoTest(t *testing.T) {
	dir, run, write := gateRepo(t)
	write("a/a.go", "package a\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")
	write("docs/readme.md", "x\n")
	run("add", "-A")
	run("commit", "-q", "-m", "docs")

	cmd, pkgs, full, err := scopedGoGate(context.Background(), dir, base)
	if err != nil || full || len(pkgs) != 0 {
		t.Fatalf("err=%v full=%v pkgs=%v", err, full, pkgs)
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

	cmd, pkgs, _, err := scopedGoGate(context.Background(), dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pkgs, []string{"."}) || !strings.HasSuffix(cmd, "go test ./.") {
		t.Errorf("pkgs=%v cmd=%s; want the root package tested", pkgs, cmd)
	}
}

// Cancelling must kill the gate's grandchildren, so a non-file writer's pipe closes and Wait returns.
func TestRunVerifyCancelKillsProcessTree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out strings.Builder
	start := time.Now()
	_ = runVerify(ctx, t.TempDir(), "sleep 30 & wait", &out, &out)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("runVerify took %s after cancellation; the orphaned grandchild held the pipe", elapsed)
	}
}

// The vault master key must not reach the gate process.
func TestRunVerifyDropsVaultKey(t *testing.T) {
	t.Setenv("HARNESS_VAULT_KEY", "sekrit")
	var out strings.Builder
	if err := runVerify(context.Background(), t.TempDir(), `echo "key=[${HARNESS_VAULT_KEY}]"`, &out, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "key=[]" {
		t.Errorf("gate saw %q, want the vault key stripped", got)
	}
}
