package core

import (
	"context"
	"os/exec"
	"testing"
)

// Given an inherited GIT_DIR naming a bogus dir, Git still reads the repo at dir (-C is not overridden).
func TestGitScrubsInheritedGitDir(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	t.Setenv("GIT_DIR", t.TempDir()+"/bogus")
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	lines, ok, err := Git(context.Background(), repo, "rev-parse", "--show-toplevel")
	if err != nil || !ok {
		t.Fatalf("Git rev-parse = ok %v err %v; GIT_DIR leaked into the call", ok, err)
	}
	if got, want := CanonPath(lines[0]), CanonPath(repo); got != want {
		t.Fatalf("toplevel = %q, want %q", got, want)
	}
}
