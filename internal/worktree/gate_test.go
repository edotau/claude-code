package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
