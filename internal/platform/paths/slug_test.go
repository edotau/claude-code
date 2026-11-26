package paths

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/hosttest"
)

// Rules 1-4: origin's repo name in any URL form, else the main checkout's folder; a worktree is its main; else @home.
func TestRepoName(t *testing.T) {
	base := t.TempDir()
	origin := func(name, url string) string {
		dir := hosttest.RepoAt(t, filepath.Join(base, name))
		if url != "" {
			hosttest.In(t, dir, "remote", "add", "origin", url)
		}
		return dir
	}
	https := origin("https", "https://user@example.invalid/acme/Widget.git")
	lane := filepath.Join(base, "lane")
	hosttest.In(t, https, "worktree", "add", "-q", "-b", "lane", lane)
	solo, soloLane := origin("solo", ""), filepath.Join(base, "solo-lane")
	hosttest.In(t, solo, "worktree", "add", "-q", "-b", "lane", soloLane)
	lone := filepath.Join(base, "lone")
	if err := os.Mkdir(lone, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ rule, dir, name, owner string }{
		{"1 https", https, "widget", "acme"},
		{"1 scp ssh", origin("scp", "git@example.invalid:acme/tool.git"), "tool", "acme"},
		{"1 local path", origin("local", "/srv/git/acme/lib.git"), "lib", "acme"},
		{"2 no origin", origin(".Dotted.Name", ""), "dotted.name", filepath.Base(base)},
		{"2 unusable origin", origin("plain", "https://example.invalid/"), "plain", filepath.Base(base)},
		{"3 linked worktree", lane, "widget", "acme"},
		{"3 linked worktree, no origin", soloLane, "solo", filepath.Base(base)},
		{"4 empty root", "", HomeRepo, ""},
	} {
		if got := RepoName(tc.dir); got.Name != tc.name || got.Owner != strings.ToLower(tc.owner) {
			t.Errorf("rule %s: RepoName(%q) = %+v, want name %q owner %q", tc.rule, tc.dir, got, tc.name, tc.owner)
		}
	}
	if !ancestorHasGit(lone) {
		if got := RepoName(lone); got != (Repo{Name: HomeRepo}) {
			t.Errorf("rule 4: RepoName(no repo) = %+v, want only @home", got)
		}
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

// Given two spellings, Then SameFile/InsideDir resolve symlinks and missing tails, folding case per FoldsCase.
func TestPathIdentity(t *testing.T) {
	hosttest.SkipOnWindows(t, "symlinks")
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if got, want := Real(filepath.Join(link, "a", "b", "c.txt")), filepath.Join(real, "a", "b", "c.txt"); got != want {
		t.Errorf("Real(link/a/b/c.txt) = %q, want %q", got, want)
	}
	for _, tc := range []struct {
		name, p, other string
		same, inside   bool
	}{
		{"through a symlink", link, dir, true, true},
		{"missing tail through a symlink", filepath.Join(link, "x", "y"), filepath.Join(real, "x", "y"), true, true},
		{"child", filepath.Join(link, "x"), dir, false, true},
		{"parent is not inside its child", dir, filepath.Join(dir, "x"), false, false},
		{"sibling sharing a prefix", filepath.Join(dir, "ab"), filepath.Join(dir, "a"), false, false},
		{"case variant", filepath.Join(dir, "A", "b"), filepath.Join(dir, "a", "b"), FoldsCase, FoldsCase},
		{"case variant under a dir", filepath.Join(dir, "SUB", "f"), filepath.Join(link, "sub"), false, FoldsCase},
	} {
		if got := SameFile(tc.p, tc.other); got != tc.same {
			t.Errorf("%s: SameFile = %v, want %v", tc.name, got, tc.same)
		}
		if got := InsideDir(tc.p, tc.other); got != tc.inside {
			t.Errorf("%s: InsideDir = %v, want %v", tc.name, got, tc.inside)
		}
	}
}

func BenchmarkCheckoutOf(b *testing.B) {
	main, linked := checkoutBenchFixture(b)
	for _, root := range []string{main, linked} {
		for _, depth := range []int{0, 8} {
			b.Run(fmt.Sprintf("%s/depth=%d", filepath.Base(root), depth), func(b *testing.B) {
				dir := root
				for range depth {
					dir = filepath.Join(dir, "nested")
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				for b.Loop() {
					if top, gotMain := CheckoutOf(dir); top != root || gotMain != main {
						b.Fatalf("CheckoutOf = %q, %q; want %q, %q", top, gotMain, root, main)
					}
				}
			})
		}
	}
}

func checkoutBenchFixture(b *testing.B) (main, linked string) {
	b.Helper()
	base := b.TempDir()
	main, linked = filepath.Join(base, "main"), filepath.Join(base, "linked")
	for _, dir := range []string{filepath.Join(main, ".git"), linked} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
	}
	for path, data := range map[string]string{
		filepath.Join(main, ".git", "HEAD"): "ref: refs/heads/main\n",
		filepath.Join(linked, ".git"):       "gitdir: " + filepath.Join(main, ".git", "worktrees", "linked") + "\n",
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	return main, linked
}

func BenchmarkReal(b *testing.B) {
	dir := b.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		b.Fatal(err)
	}
	for _, kind := range []string{"existing", "symlink", "missing-tail"} {
		b.Run(kind, func(b *testing.B) {
			path, expected := dir, want
			if kind != "existing" {
				if runtime.GOOS == "windows" {
					b.Skip("symlinks require Windows privileges")
				}
				path = filepath.Join(b.TempDir(), "link")
				if err := os.Symlink(dir, path); err != nil {
					b.Fatal(err)
				}
			}
			if kind == "missing-tail" {
				path, expected = filepath.Join(path, "a", "b", "c"), filepath.Join(expected, "a", "b", "c")
			}
			b.ReportAllocs()
			for b.Loop() {
				if got := Real(path); got != expected {
					b.Fatalf("Real = %q, want %q", got, expected)
				}
			}
		})
	}
}

func BenchmarkGitTopRepeatedCwd(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("git not installed")
	}
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	repo := b.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		b.Fatalf("git init: %v: %s", err, out)
	}
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		// two calls for the same cwd in one process: each currently spawns git
		for range 2 {
			got, err := filepath.EvalSymlinks(GitTop(ctx, repo))
			if err != nil || got != want {
				b.Fatalf("GitTop = %q (%v), want %q", got, err, want)
			}
		}
	}
}
