package statusline

import (
	"os"
	"path/filepath"
	"testing"
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

// gitFixture builds a repo dir whose .git is a directory (gitfile "") or a gitdir file ("relative", "absolute",
// "empty"), writes head as its HEAD unless head is "missing", and returns a nested cwd inside it.
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

// checkoutOf maps a subdir or a linked worktree to the main checkout (naming the worktree), and leaves a
// non-repo cwd as is.
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
	dir := t.TempDir()
	t.Setenv("STATUSLINE_GIT_MARKER", filepath.Join(dir, "listed"))
	script := "#!/bin/sh\nif test -f \"$STATUSLINE_GIT_MARKER\"; then\n exec sleep 5\nfi\n: > \"$STATUSLINE_GIT_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	adds, dels, ok := treeDiffStat(dir)
	if ok || adds != 0 || dels != 0 {
		t.Fatalf("deadline expired: treeDiffStat = (%d, %d, %v), want (0, 0, false)", adds, dels, ok)
	}
}
