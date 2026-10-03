package crossgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Source is a parsed agent (agents/<name>.md) or skill (skills/<dir>/SKILL.md); dirName is the file stem or skill dir.
type Source struct {
	dirName     string
	frontmatter map[string]any
	body        string
}

// Name is the output basename the writers use: frontmatter `name`, else the dir name.
func (s Source) Name() string {
	if v, ok := s.frontmatter["name"].(string); ok && v != "" {
		return v
	}
	return s.dirName
}

// DiscoverAgents reads the flat agents/<name>.md layout Claude Code and VS Code both read, sorted by name;
// a frontmatter `name` that differs from the file stem is an error. Symlinks are skipped, as for skills.
func DiscoverAgents(agentsDir string) ([]Source, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil, nil
	}
	var agents []Source
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || !e.Type().IsRegular() || stem == "README" {
			continue
		}
		text, err := os.ReadFile(filepath.Join(agentsDir, e.Name()))
		if err != nil {
			continue
		}
		fm, body := ParseFrontmatter(string(text))
		a := Source{dirName: stem, frontmatter: fm, body: body}
		if a.Name() != stem {
			return nil, fmt.Errorf("agents/%s.md: frontmatter name %q != file name", stem, a.Name())
		}
		agents = append(agents, a)
	}
	return agents, nil
}

// DiscoverSkills reads skills/<dir>/SKILL.md sorted by dir. Symlinked dirs are skipped: they point into
// ~/.agents/skills, which Gemini CLI already reads, so projecting them would duplicate every name.
func DiscoverSkills(skillsDir string) []Source {
	return discoverSources(skillsDir, "SKILL.md")
}

// validateUniqueNames rejects same-kind sources that would write to the same output name.
func validateUniqueNames(kind string, sources []Source) error {
	seen := make(map[string]string, len(sources))
	for _, source := range sources {
		name := source.Name()
		if first, ok := seen[name]; ok {
			return fmt.Errorf("duplicate %s name %q in %s and %s", kind, name, first, source.dirName)
		}
		seen[name] = source.dirName
	}
	return nil
}

// discoverSources returns every REAL child dir of root (Lstat, not a symlink) holding a regular <filename>.
func discoverSources(root, filename string) []Source {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.Type().IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)

	var out []Source
	for _, d := range dirs {
		defPath := filepath.Join(root, d, filename)
		if info, err := os.Stat(defPath); err != nil || !info.Mode().IsRegular() {
			continue
		}
		text, err := os.ReadFile(defPath)
		if err != nil {
			continue
		}
		fm, body := ParseFrontmatter(string(text))
		out = append(out, Source{dirName: d, frontmatter: fm, body: body})
	}
	return out
}

// readTextNoFollow reads a file, refusing to follow a symlink at the final component (TOCTOU guard).
func readTextNoFollow(path string) (string, bool) {
	f, err := openNoFollow(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", false
	}
	return string(data), true
}
