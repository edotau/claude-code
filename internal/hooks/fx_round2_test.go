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
