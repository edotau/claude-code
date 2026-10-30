package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestScopedGoGate covers changed-package selection, present registry packages, non-Go and deleted paths, and go.mod fallback.
func TestScopedGoGate(t *testing.T) {
	dir := gitRepo(t)
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/old/old.go")
	write("internal/hooks/hooks.go") // one registry package present; the other four are absent and skipped
	run("add", "-A")
	run("commit", "-q", "-m", "old")
	base := strings.TrimSpace(func() string { o, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); return string(o) }())

	write("internal/foo/foo.go")
	write("internal/foo/testdata/in.txt")
	write("docs/readme.md")
	if err := os.Remove(filepath.Join(dir, "internal/old/old.go")); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "change")

	cmd, pkgs, full, err := ScopedGoGate(context.Background(), dir, base)
	if err != nil || full {
		t.Fatalf("err=%v full=%v", err, full)
	}
	want := []string{"internal/foo", "internal/hooks"}
	for _, p := range want {
		if !strings.Contains(cmd, "./"+p) {
			t.Errorf("gate missing %s: %s", p, cmd)
		}
	}
	if len(pkgs) != len(want) || strings.Contains(cmd, "internal/old") || strings.Contains(cmd, "docs") ||
		strings.Contains(cmd, "internal/cli") {
		t.Errorf("pkgs=%v cmd=%s", pkgs, cmd)
	}
	if !strings.HasPrefix(cmd, "go build ./... && go vet ./... && go test ") {
		t.Errorf("cmd=%s", cmd)
	}

	write("go.mod")
	run("add", "-A")
	run("commit", "-q", "-m", "mod")
	cmd, _, full, err = ScopedGoGate(context.Background(), dir, base)
	if err != nil || !full || !strings.HasSuffix(cmd, "go test ./...") {
		t.Fatalf("go.mod change: cmd=%s full=%v err=%v", cmd, full, err)
	}
}
