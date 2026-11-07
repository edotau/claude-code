package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// Slug lowercases s into one safe path element: runs outside [a-z0-9]+keep become one '-', edges trimmed,
// capped at max bytes (0 = uncapped); an empty result yields fallback.
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

// RepoSlug is root's tag for repo-scoped artifacts; the config dir itself is "" so its artifacts sit untagged.
// A linked worktree is tagged by its main checkout, so every worktree of a repo shares one bank instead of each
// branch directory ("main", "fix-x") minting its own and colliding across repos.
func RepoSlug(root string) string {
	main := MainCheckout(root)
	if root == "" || SamePath(root, ConfigDir()) || SamePath(main, ConfigDir()) {
		return ""
	}
	return Slug(filepath.Base(filepath.Clean(main)), "._-", 0, "")
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

// CheckoutOf walks up from dir to the nearest directory holding .git: top is that dir, main its MainCheckout.
// ("", "") when dir is not inside a repo; a linked worktree is top != "" && main != top. An empty .git dir (a stray
// /tmp/.git) is no repo, as to git.
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

// hasGit reports whether path is a gitfile or a non-empty .git directory.
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

// maxLinkDepth bounds dangling-symlink chasing in CanonDeep.
const maxLinkDepth = 40

// CanonDeep resolves symlinks in p's deepest existing ancestor and rejoins the missing tail unresolved;
// a dangling symlink is followed to its target.
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
