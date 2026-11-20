package paths

import (
	"path/filepath"
	"strings"
)

// AnchorPath anchors a relative path to cwd; an empty path or cwd leaves it unchanged.
func AnchorPath(path, cwd string) string {
	if path != "" && !filepath.IsAbs(path) && cwd != "" {
		return filepath.Join(cwd, path)
	}
	return path
}

// RelWithin is abs relative to root; false when abs escapes root.
func RelWithin(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
