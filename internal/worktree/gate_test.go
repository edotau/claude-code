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

	cmd, pkgs, full, err := ScopedGoGate(context.Background(), dir, base)
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
	cmd, _, full, err = ScopedGoGate(context.Background(), dir, base)
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

	_, pkgs, _, err := ScopedGoGate(context.Background(), dir, base)
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

	_, pkgs, full, err := ScopedGoGate(context.Background(), dir, base)
	if err != nil || full {
		t.Fatalf("err=%v full=%v", err, full)
	}
	if want := []string{"emb", "emb/sub", "mem"}; !slices.Equal(pkgs, want) {
		t.Errorf("pkgs=%v want %v", pkgs, want)
	}

	write("tools/run.sh", "#!/bin/sh\n")
	run("add", "-A")
	run("commit", "-q", "-m", "script")
	if cmd, _, full, err := ScopedGoGate(context.Background(), dir, base); err != nil || !full || cmd != fullGoGate {
		t.Fatalf("unowned non-markdown file: cmd=%s full=%v err=%v", cmd, full, err)
	}
}

// A cancelled ctx must kill the long-running gate promptly instead of letting it finish.
func TestRunVerifyCancelledByContextStopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := RunVerify(ctx, t.TempDir(), "sleep 30", io.Discard, io.Discard)
	if err == nil {
		t.Fatal("RunVerify: want an error for an already-cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("RunVerify took %s after cancellation, want it to stop promptly", elapsed)
	}
}

// Precedence: --no-verify, then --verify, then detection (Makefile test target beats go.mod); nothing found refuses.
func TestChooseVerifyGatePrecedence(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := ChooseVerifyGate(dir, "", false); err == nil {
		t.Fatal("empty dir without --no-verify: want a refusal")
	}
	if g, _, err := ChooseVerifyGate(dir, "x", true); g != "" || err != nil {
		t.Fatalf("--no-verify = (%q, %v), want skip", g, err)
	}
	if g, s, _ := ChooseVerifyGate(dir, "x", false); g != "x" || s != "flag" {
		t.Fatalf("--verify = (%q, %q)", g, s)
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n"), 0o644)
	if _, s, _ := ChooseVerifyGate(dir, "", false); s != "go.mod" {
		t.Fatalf("go.mod source = %q", s)
	}
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("test:\n\ttrue\n"), 0o644)
	if g, s, _ := ChooseVerifyGate(dir, "", false); g != "make test" || s != "Makefile" {
		t.Fatalf("Makefile = (%q, %q)", g, s)
	}
}
