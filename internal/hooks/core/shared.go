package core

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// GitCommitCmd matches a shell segment that runs `git commit`.
	GitCommitCmd = regexp.MustCompile(`\bgit\b[^;&|\n]*\bcommit\b`)
	// CommitLine matches the `[branch sha] subject` line git prints for a landed commit.
	CommitLine = regexp.MustCompile(`(?m)^\[(?:detached HEAD|[^\]\s]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\] `)
)

// RepoRootOf climbs from dir's nearest existing ancestor to the first dir holding a `.git` entry, or "".
func RepoRootOf(dir string) string {
	for dir != "" {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// UnderDir reports whether path is dir itself or below it (both already canonical).
func UnderDir(dir, path string) bool {
	return dir != "" && (path == dir || strings.HasPrefix(path, dir+string(filepath.Separator)))
}
