package paths

import (
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
func RepoSlug(root string) string {
	if root == "" || SamePath(root, ConfigDir()) {
		return ""
	}
	return Slug(filepath.Base(filepath.Clean(root)), "._-", 0, "")
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
