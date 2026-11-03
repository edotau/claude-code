package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A linked worktree's bank tag is its main checkout's name, never the branch dir it was checked out into.
func TestRepoSlugUsesMainCheckoutForLinkedWorktree(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := filepath.Join(t.TempDir(), "workspaces", "my-site", "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ptr := "gitdir: /Users/x/src/github.com/me/my-site/.git/worktrees/23710-abc\n"
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte(ptr), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RepoSlug(root); got != "my-site" {
		t.Errorf("RepoSlug(worktree) = %q, want my-site", got)
	}

	primary := filepath.Join(t.TempDir(), "Other.Repo")
	if err := os.MkdirAll(filepath.Join(primary, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := RepoSlug(primary); got != "other.repo" {
		t.Errorf("RepoSlug(primary) = %q, want other.repo", got)
	}

	rel := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, ".git"), []byte("gitdir: ../repo-b/.git/worktrees/wt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RepoSlug(rel); got != "repo-b" {
		t.Errorf("RepoSlug(relative gitdir) = %q, want repo-b", got)
	}
}

func TestMainCheckoutPointerFallbacks(t *testing.T) {
	for _, tc := range []struct{ name, pointer, want string }{
		{"malformed", "not a git pointer", ""},
		{"empty", "gitdir: ", ""},
		{"separate git dir", "gitdir: ../gitdata", ""},
		{"submodule", "gitdir: ../main/.git/modules/sub", ""},
		{"relative worktree", "gitdir: ../main/.git/worktrees/wt", "main"},
		{"missing target", "gitdir: /absent/main/.git/worktrees/wt", "/absent/main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, ".git"), []byte(tc.pointer), 0o644); err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == "" {
				want = root
			} else if !filepath.IsAbs(want) {
				want = filepath.Join(filepath.Dir(root), want)
			}
			if got := MainCheckout(root); got != want {
				t.Errorf("MainCheckout(%q) = %q, want %q", root, got, want)
			}
		})
	}
	if got := MainCheckout(""); got != "" {
		t.Errorf("MainCheckout(empty) = %q, want empty", got)
	}
}

func TestMainCheckoutGitOutput(t *testing.T) {
	for _, tc := range []struct{ name, output, want string }{
		{"empty", "", ""}, {"missing common", "/repo", ""},
		{"missing top", "\n/repo/.git", ""}, {"extra line", "/repo\n/repo/.git\nextra", ""},
		{"main", "/repo\n/repo/.git\n", "/repo"},
		{"worktree", "/wt\n/repo/.git\n", "/repo"},
		{"separate git dir", "/repo\n/gitdata\n", "/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := func(root string, args ...string) (string, error) {
				if root != "input" || strings.Join(args, " ") != "rev-parse --path-format=absolute --show-toplevel --git-common-dir" {
					t.Fatalf("unexpected git probe: %q %q", root, args)
				}
				return tc.output, nil
			}
			if got := MainCheckout("input", probe); got != tc.want {
				t.Errorf("MainCheckout git output %q = %q, want %q", tc.output, got, tc.want)
			}
		})
	}
	probe := func(string, ...string) (string, error) { return "", os.ErrNotExist }
	if got := MainCheckout("input", probe); got != "" {
		t.Errorf("MainCheckout failed git = %q, want empty", got)
	}
}

func TestRepoSlugConfigWorktree(t *testing.T) {
	main := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", main)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: "+main+"/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RepoSlug(root); got != "" {
		t.Errorf("RepoSlug(config worktree) = %q, want empty", got)
	}
}

// The inverse: the config dir is itself a linked worktree of another checkout.
func TestRepoSlugConfigDirIsWorktree(t *testing.T) {
	main := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.WriteFile(filepath.Join(cfg, ".git"), []byte("gitdir: "+main+"/.git/worktrees/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := RepoSlug(cfg); got != "" {
		t.Errorf("RepoSlug(config dir worktree) = %q, want empty", got)
	}
}

func TestCheckoutOf(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "repo")
	sub := filepath.Join(main, "internal", "pkg")
	wt := filepath.Join(base, "worktrees", "x")
	subm := filepath.Join(base, "subm")
	lone := filepath.Join(base, "lone")
	dirs := []string{filepath.Join(main, ".git", "worktrees", "x"), sub, filepath.Join(wt, "cmd"), filepath.Join(subm, "pkg"), lone}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, ptr string) {
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(ptr), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(wt, "gitdir: "+filepath.Join(main, ".git", "worktrees", "x")+"\n")
	write(subm, "gitdir: "+filepath.Join(main, ".git", "modules", "subm")+"\n")
	for _, tc := range []struct{ name, dir, top, main string }{
		{"main root", main, main, main},
		{"main subdir", sub, main, main},
		{"linked worktree", wt, wt, main},
		{"worktree subdir", filepath.Join(wt, "cmd"), wt, main},
		{"submodule gitfile", filepath.Join(subm, "pkg"), subm, subm},
	} {
		if top, m := CheckoutOf(tc.dir); top != tc.top || m != tc.main {
			t.Errorf("%s: CheckoutOf(%q) = %q, %q; want %q, %q", tc.name, tc.dir, top, m, tc.top, tc.main)
		}
	}
	// the temp tree may itself sit under a repo, so only assert "no repo" when none encloses it
	if !ancestorHasGit(lone) {
		if top, m := CheckoutOf(lone); top != "" || m != "" {
			t.Errorf("no repo: CheckoutOf = %q, %q; want empty", top, m)
		}
	}
}

// An empty .git directory is not a repo, so a stray one in an ancestor (/tmp/.git) cannot capture a plain directory.
func TestCheckoutOfSkipsEmptyGitDir(t *testing.T) {
	base := t.TempDir()
	lone := filepath.Join(base, "lone")
	if err := os.MkdirAll(filepath.Join(lone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(lone, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{lone, filepath.Join(lone, "sub")} {
		if top, m := CheckoutOf(dir); top == lone || m == lone {
			t.Errorf("CheckoutOf(%q) = %q, %q; an empty .git must not count", dir, top, m)
		}
	}
}

// ancestorHasGit reports whether dir or any parent holds a .git entry (a stray /tmp/.git included).
func ancestorHasGit(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}
