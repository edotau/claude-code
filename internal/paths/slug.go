package paths

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

// Slug lowercases s into one safe path element
func Slug(s, keep string, max int, fallback string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', strings.ContainsRune(keep, r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if max > 0 && len(slug) > max {
		slug = strings.Trim(slug[:max], "-")
	}
	if slug == "" {
		return fallback
	}
	return slug
}

// ProjectSlug mirrors Claude Code's projects/<slug> naming: every non-alphanumeric byte becomes '-'.
func ProjectSlug(root string) string {
	b := []byte(filepath.Clean(root))
	for i, c := range b {
		if ('a' > c || c > 'z') && ('A' > c || c > 'Z') && ('0' > c || c > '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// RepoSlug is root's tag for repo-scoped artifacts
func RepoSlug(root string) string {
	main := MainCheckout(root)
	if root == "" || SamePath(root, ConfigDir()) || SamePath(main, ConfigDir()) {
		return ""
	}
	return Slug(filepath.Base(filepath.Clean(main)), "._-", 0, "")
}

// GitTop is cwd's `git rev-parse --show-toplevel`, "" when git fails or prints nothing (not a repo).
func GitTop(ctx context.Context, cwd string) string {
	out, err := proc.RunGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// MainCheckout resolves a worktree pointer lexically; with a git runner it probes enclosing repos, failing to "".
func MainCheckout(root string, gitOut ...func(string, ...string) (string, error)) string {
	if len(gitOut) > 0 {
		out, err := gitOut[0](root, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
		if err != nil {
			return ""
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 || lines[0] == "" {
			return ""
		}
		top, common := filepath.Clean(lines[0]), filepath.Clean(lines[1])
		if filepath.Base(common) == ".git" {
			return filepath.Dir(common)
		}
		return top
	}
	data, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return root
	}
	ptr, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return root
	}
	ptr = strings.TrimSpace(ptr)
	if !filepath.IsAbs(ptr) {
		ptr = filepath.Join(root, ptr)
	}
	ptr = filepath.Clean(ptr)
	marker := string(filepath.Separator) + filepath.Join(".git", "worktrees") + string(filepath.Separator)
	if i := strings.Index(ptr, marker); i > 0 {
		return ptr[:i]
	}
	return root
}

// CheckoutOf walks up from dir to the nearest directory holding .git
func CheckoutOf(dir string) (top, main string) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", ""
	}
	for {
		if hasGit(filepath.Join(dir, ".git")) {
			return dir, MainCheckout(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

func hasGit(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		return true
	}
	d, err := os.Open(path)
	if err != nil {
		return false
	}
	defer d.Close()
	names, _ := d.Readdirnames(1)
	return len(names) > 0
}

// FoldsCase holds where the default filesystem is case-insensitive (macOS, Windows); Linux compares byte-exact.
const FoldsCase = runtime.GOOS != "linux"

// FoldCase lowercases p where FoldsCase holds, for a caller that matches a glob or prefix itself.
func FoldCase(p string) string {
	if !FoldsCase {
		return p
	}
	return strings.ToLower(p)
}

// Real is p made absolute and symlink-resolved through its deepest existing ancestor (see CanonDeep).
func Real(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	return CanonDeep(p)
}

// SameFile reports whether a and b name one file: both Real, then case-folded per FoldsCase.
func SameFile(a, b string) bool { return FoldCase(Real(a)) == FoldCase(Real(b)) }

// InsideDir reports whether p is dir or under it, compared as SameFile compares.
func InsideDir(p, dir string) bool { return Within(FoldCase(Real(p)), FoldCase(Real(dir))) }

// SamePath compares two paths after Clean and symlink resolution (unresolvable paths compare cleaned).
func SamePath(a, b string) bool {
	return Canon(a) == Canon(b)
}

// Canon cleans p and resolves its symlinks; an unresolvable path (e.g. already deleted) stays cleaned.
func Canon(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

const maxLinkDepth = 40

// CanonDeep resolves symlinks in p's deepest existing ancestor and rejoins the missing tail unresolved
func CanonDeep(p string) string { return canonDeep(p, 0) }

func canonDeep(p string, depth int) string {
	p = filepath.Clean(p)
	tail := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, tail)
		}
		if t, ok := danglingTarget(cur, depth); ok {
			return filepath.Join(canonDeep(t, depth+1), tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// danglingTarget returns the absolute target of cur when it is a symlink whose target is missing.
func danglingTarget(cur string, depth int) (string, bool) {
	if depth >= maxLinkDepth {
		return "", false
	}
	if fi, err := os.Lstat(cur); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	t, err := os.Readlink(cur)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(canonDeep(filepath.Dir(cur), depth+1), t)
	}
	return t, true
}
