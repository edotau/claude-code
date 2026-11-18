package statusline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestGitBranchFast(t *testing.T) {
	t.Setenv("GIT_DIR", "")
	for _, tc := range []struct {
		name, gitfile, head, want string
		ok                        bool
	}{
		{name: "directory", head: "ref: refs/heads/feature/statusline\n", want: "feature/statusline", ok: true},
		{name: "relative gitfile", gitfile: "relative", head: "ref: refs/heads/worktree\n", want: "worktree", ok: true},
		{name: "absolute gitfile", gitfile: "absolute", head: "ref: refs/heads/worktree\n", want: "worktree", ok: true},
		{name: "detached", head: "0123456789abcdef\n", want: "HEAD", ok: true},
		{name: "empty HEAD", head: "", want: "HEAD", ok: true},
		{name: "other symbolic ref", head: "ref: refs/tags/v1\n"},
		{name: "missing HEAD", head: "missing"},
		{name: "reftable HEAD", head: "ref: refs/heads/.invalid\n"},
		{name: "control character", head: "ref: refs/heads/a\x1b[31mb\n"},
		{name: "oversized HEAD", head: "ref: refs/heads/" + strings.Repeat("a", headReadCap)},
		{name: "empty gitfile", gitfile: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := gitFixture(t, tc.gitfile, tc.head)
			if got, ok := gitBranchFast(cwd); got != tc.want || ok != tc.ok {
				t.Fatalf("gitBranchFast = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
	t.Setenv("GIT_DIR", t.TempDir())
	if got, ok := gitBranchFast(t.TempDir()); got != "" || ok {
		t.Fatalf("GIT_DIR override = %q, %v; want fallback", got, ok)
	}
}

// gitFixture builds a repo dir (.git as directory or gitdir file variant), writes head, and returns a nested cwd.
func gitFixture(t *testing.T, gitfile, head string) string {
	t.Helper()
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if gitfile != "" {
		gitDir = filepath.Join(dir, "metadata")
		target := gitDir
		switch gitfile {
		case "relative":
			target = "metadata"
		case "empty":
			target = ""
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+target+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if head != "missing" {
		if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(head), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cwd := filepath.Join(dir, "nested", "deeper")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return cwd
}

// checkoutOf maps a subdir or linked worktree to the main checkout and leaves a non-repo cwd as is.
func TestCheckoutOf(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "repo")
	sub := filepath.Join(main, "internal", "pkg")
	wt := filepath.Join(base, "worktrees", "x")
	lone := filepath.Join(base, "lone")
	for _, d := range []string{filepath.Join(main, ".git", "worktrees", "x"), sub, filepath.Join(wt, "cmd"), lone} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitfile := "gitdir: " + filepath.Join(main, ".git", "worktrees", "x") + "\n"
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte(gitfile), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, cwd, root, worktree string }{
		{"repo root", main, main, ""},
		{"subdir", sub, main, ""},
		{"linked worktree", wt, main, wt},
		{"worktree subdir", filepath.Join(wt, "cmd"), main, wt},
		{"no repo", lone, lone, ""},
	} {
		if root, worktree := checkoutOf(tc.cwd); root != tc.root || worktree != tc.worktree {
			t.Errorf("%s: checkoutOf(%q) = %q, %q; want %q, %q", tc.name, tc.cwd, root, worktree, tc.root, tc.worktree)
		}
	}
}

func TestTreeDiffDeadlineFallsBack(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	orig := gitDiffTimeout
	gitDiffTimeout = 50 * time.Millisecond
	t.Cleanup(func() { gitDiffTimeout = orig })
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$3\" in\n ls-files) exec sleep 5 ;;\n --no-optional-locks) printf '7\\t3\\ttracked\\n' ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	adds, dels, ok := treeDiffStat(dir)
	if ok || adds != 0 || dels != 0 {
		t.Fatalf("deadline expired: treeDiffStat = (%d, %d, %v), want (0, 0, false)", adds, dels, ok)
	}
}

// The velocity counts the branch's committed and uncommitted lines since the default branch, not just uncommitted.
func TestTreeDiffStatCountsBranchSinceBase(t *testing.T) {
	repo := hosttest.Repo(t)
	hosttest.In(t, repo, "checkout", "-q", "-b", "feature")
	hosttest.WriteRel(t, repo, "a.txt", "1\n2\n3\n")
	hosttest.In(t, repo, "add", "a.txt")
	hosttest.In(t, repo, "commit", "-q", "-m", "committed")
	hosttest.WriteRel(t, repo, "a.txt", "1\n2\n3\n4\n5\n")
	if adds, dels, ok := treeDiffStat(repo); !ok || adds != 5 || dels != 0 {
		t.Fatalf("treeDiffStat = (%d, %d, %v), want (5, 0, true)", adds, dels, ok)
	}
}

// originHeadFast equals `git symbolic-ref --short refs/remotes/origin/HEAD` wherever it answers.
func TestOriginHeadFastMatchesGit(t *testing.T) {
	t.Setenv("GIT_DIR", "") // registers the restore; an empty GIT_DIR breaks git, so unset it for the spawn
	if err := os.Unsetenv("GIT_DIR"); err != nil {
		t.Fatal(err)
	}
	spawn := func(cwd string) string {
		return gitLine(t.Context(), cwd, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	}
	repo := hosttest.Repo(t)
	check := func(name, cwd string, wantFast bool) {
		t.Helper()
		got, ok := originHeadFast(cwd)
		if ok != wantFast || (ok && got != spawn(cwd)) {
			t.Errorf("%s: originHeadFast = %q, %v; spawn %q; want fast=%v", name, got, ok, spawn(cwd), wantFast)
		}
	}
	check("no origin", repo, false)
	hosttest.In(t, repo, "remote", "add", "origin", repo)
	hosttest.In(t, repo, "fetch", "-q", "origin")
	if err := os.Remove(filepath.Join(repo, ".git", "refs", "remotes", "origin", "HEAD")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	check("origin HEAD unset", repo, false)
	hosttest.In(t, repo, "remote", "set-head", "origin", "-a")
	if spawn(repo) == "" {
		t.Skip("git did not set origin/HEAD")
	}
	check("normal clone", repo, true)
	check("subdir", filepath.Join(repo, "."), true)
	wt := filepath.Join(t.TempDir(), "wt")
	hosttest.In(t, repo, "worktree", "add", "-q", "-b", "side", wt)
	check("linked worktree", wt, true)
	amb := "origin/" + strings.TrimPrefix(spawn(repo), "origin/")
	hosttest.In(t, repo, "branch", amb)
	check("ambiguous short name", repo, false)
	hosttest.In(t, repo, "pack-refs", "--all")
	check("ambiguous branch only in packed-refs", repo, false)
	hosttest.In(t, repo, "branch", "-D", amb)
	check("branch deleted", repo, true)
	hosttest.In(t, repo, "tag", amb)
	hosttest.In(t, repo, "pack-refs", "--all")
	check("ambiguous tag only in packed-refs", repo, false)
}
