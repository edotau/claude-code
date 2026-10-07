package memory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// NativeDirs lists Claude Code's auto-memory dirs recall searches: the harness's own (user-wide facts), then root's.
func NativeDirs(root string) []string {
	roots := []string{paths.ConfigDir(), root}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil && root != "" {
		roots = append(roots, GitToplevel(root)) // a subdir's sessions file under the repo top; forks only off-toplevel
	}
	var out []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		if d := filepath.Join(paths.ConfigDir(), "projects", projectSlug(r), "memory"); !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// projectSlug mirrors Claude Code's projects/<slug> naming: every non-alphanumeric byte becomes '-'.
func projectSlug(root string) string {
	b := []byte(filepath.Clean(root))
	for i, c := range b {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// topicPaths lists dir's topic files; MEMORY.md is the injected pointer index, not a topic.
func topicPaths(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") && e.Name() != "MEMORY.md" {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// topicFrontmatter splits a topic file into name, description and body; false without a closed block and a name.
func topicFrontmatter(raw string) (name, desc, body string, ok bool) {
	lines := strings.Split(raw, "\n")
	if strings.TrimSpace(lines[0]) != "---" {
		return "", "", "", false
	}
	for i, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			if name == "" {
				return "", "", "", false
			}
			return name, desc, strings.TrimSpace(strings.Join(lines[i+2:], "\n")), true
		}
		if v, found := strings.CutPrefix(line, "name:"); found {
			name = strings.Trim(strings.TrimSpace(v), `"'`)
		} else if v, found := strings.CutPrefix(line, "description:"); found {
			desc = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return "", "", "", false
}
