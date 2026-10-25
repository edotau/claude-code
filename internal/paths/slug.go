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

// MainCheckout resolves a linked worktree's `gitdir: <main>/.git/worktrees/<id>` pointer file to <main>; any
// other root (a primary checkout, a plain dir) is returned unchanged.
func MainCheckout(root string) string {
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
// ("", "") when dir is not inside a repo; a linked worktree is top != "" && main != top.
func CheckoutOf(dir string) (top, main string) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, MainCheckout(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

// SamePath compares two paths after Clean and symlink resolution (unresolvable paths compare cleaned).
func SamePath(a, b string) bool {
	return canon(a) == canon(b)
}

func canon(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
