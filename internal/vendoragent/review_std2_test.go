package vendoragent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// std2Commit rewrites a.txt to content and commits it.
func std2Commit(t *testing.T, dir, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", msg)
}

// std2OriginHEAD points refs/remotes/origin/HEAD at origin/<branch>, both at the current HEAD, with no network remote.
func std2OriginHEAD(t *testing.T, dir, branch string) {
	t.Helper()
	runGit(t, dir, "update-ref", "refs/remotes/origin/"+branch, "HEAD")
	runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+branch)
}

// review --pr diffs against origin/<default>, so commits sitting only on a stale-ahead local default are in scope.
func TestStd2DiffPRUsesOriginDefaultBranch(t *testing.T) {
	dir := initRepo(t)
	std2OriginHEAD(t, dir, "main")
	std2Commit(t, dir, "one\nunpushed-on-local-main\n", "local only")
	runGit(t, dir, "checkout", "-q", "-b", "feature")
	std2Commit(t, dir, "one\nunpushed-on-local-main\nfeature-line\n", "feature")

	out, err := Diff(context.Background(), dir, Scope{Kind: "pr"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	for _, want := range []string{"+feature-line", "+unpushed-on-local-main"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("pr diff missing %q: the base must be origin/main, not the local main:\n%s", want, out)
		}
	}
}

// A default branch named like a flag must reach git behind --end-of-options, not be parsed as an option.
func TestStd2DiffPRPassesEndOfOptions(t *testing.T) {
	dir := initRepo(t)
	std2OriginHEAD(t, dir, "-main")
	runGit(t, dir, "checkout", "-q", "-b", "feature")
	std2Commit(t, dir, "one\nfeature-line\n", "feature")

	out, err := Diff(context.Background(), dir, Scope{Kind: "pr"})
	if err != nil {
		t.Fatalf("Diff with default branch %q: %v", "-main", err)
	}
	if !strings.Contains(string(out), "feature-line") {
		t.Errorf("pr diff missing the feature commit:\n%s", out)
	}
}
