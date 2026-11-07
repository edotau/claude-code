package gittest

import (
	"strings"
	"testing"
)

func TestInIgnoresLeakedGitDir(t *testing.T) {
	outer := Repo(t)
	inner := Repo(t)
	t.Setenv("GIT_DIR", outer+"/.git")

	In(t, inner, "branch", "leak-probe")

	if got := In(t, inner, "branch", "--list", "leak-probe"); !strings.Contains(got, "leak-probe") {
		t.Fatalf("branch missing from the -C repo: %q", got)
	}
	t.Setenv("GIT_DIR", "")
	if got := In(t, outer, "branch", "--list", "leak-probe"); strings.Contains(got, "leak-probe") {
		t.Fatalf("branch leaked into the repo named by GIT_DIR: %q", got)
	}
}
