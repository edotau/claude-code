package hooks

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each sha of a chained `git -C A commit && git -C B commit` is matched to the repo that holds it.
func TestCommitRepoDirTriesEveryDashC(t *testing.T) {
	root := t.TempDir()
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	var repos, shas []string
	for _, n := range []string{"repoA", "repoB"} {
		d := filepath.Join(root, n)
		git(root, "init", "-q", d)
		git(d, "commit", "-q", "--allow-empty", "-m", n)
		repos, shas = append(repos, d), append(shas, git(d, "rev-parse", "HEAD"))
	}
	cmd := "git -C " + repos[0] + " commit -m a && git -C " + repos[1] + " commit -m b"
	for i, sha := range shas {
		if got := commitRepoDir(context.Background(), root, cmd, sha); got != repos[i] {
			t.Errorf("commitRepoDir for %s = %q, want %q", filepath.Base(repos[i]), got, repos[i])
		}
	}
}

// Linked worktrees share objects: the commit belongs to the worktree whose HEAD it is, and a non-commit -C is no candidate.
func TestCommitRepoDirLinkedWorktrees(t *testing.T) {
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	a, b, other := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "other")
	run(root, "init", "-q", a)
	run(a, "commit", "-q", "--allow-empty", "-m", "base")
	run(a, "worktree", "add", "-q", "-b", "wt", b)
	run(root, "init", "-q", other)
	run(a, "commit", "-q", "--allow-empty", "-m", "a")
	run(b, "commit", "-q", "--allow-empty", "-m", "b")
	shaA, shaB := run(a, "rev-parse", "HEAD"), run(b, "rev-parse", "HEAD")
	cmd := "git -C " + a + " commit -m a && git -C " + b + " commit -m b"
	if got := commitRepoDir(context.Background(), root, cmd, shaB); got != b {
		t.Errorf("sha made in worktree b resolved to %q, want %q", got, b)
	}
	if got := commitRepoDir(context.Background(), root, cmd, shaA); got != a {
		t.Errorf("sha made in a resolved to %q, want %q", got, a)
	}
	cmd = "git -C " + a + " status && git commit -m b"
	if got := commitRepoDir(context.Background(), b, cmd, shaB); got != b {
		t.Errorf("a non-commit -C outranked cwd: got %q, want %q", got, b)
	}
}
