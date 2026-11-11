package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
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

// DiscoverAgents reads agents/<name>.md sorted by name; a frontmatter name differing from the stem is an error.
func DiscoverAgents(agentsDir string) ([]Source, error) {
	entries, err := readDirOrNil(agentsDir)
	if err != nil {
		return nil, err
	}
	var agents []Source
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || !e.Type().IsRegular() || stem == "README" {
			continue
		}
		a, err := loadSource(filepath.Join(agentsDir, e.Name()), stem)
		if err != nil {
			return nil, err
		}
		if a.Name() != stem {
			return nil, fmt.Errorf("agents/%s.md: frontmatter name %q != file name", stem, a.Name())
		}
		agents = append(agents, a)
	}
	return agents, nil
}

// discoverSkills reads skills/<dir>/SKILL.md, including symlinked dirs resolving inside skills/; skills/synced is excluded.
func discoverSkills(skillsDir string) ([]Source, error) {
	return discoverSources(skillsDir, "SKILL.md")
}

// validateUniqueNames rejects skills that would write to the same output name.
func validateUniqueNames(sources []Source) error {
	seen := make(map[string]string, len(sources))
	for _, source := range sources {
		name := source.Name()
		if first, ok := seen[name]; ok {
			return fmt.Errorf("duplicate skill name %q in %s and %s", name, first, source.dirName)
		}
		seen[name] = source.dirName
	}
	return nil
}

// readDirOrNil lists dir, treating a missing dir as empty.
func readDirOrNil(dir string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	return entries, nil
}

// discoverSources returns every child dir of root holding a regular <filename>; read errors propagate.
func discoverSources(root, filename string) ([]Source, error) {
	entries, err := readDirOrNil(root)
	if err != nil {
		return nil, err
	}
	var out []Source
	for _, e := range entries {
		d := e.Name()
		if d == "synced" {
			continue
		}
		if !e.Type().IsDir() && (e.Type()&os.ModeSymlink == 0 || !symlinkInside(root, filepath.Join(root, d))) {
			continue
		}
		defPath := filepath.Join(root, d, filename)
		info, err := os.Stat(defPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat %s: %w", defPath, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		src, err := loadSource(defPath, d)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, nil
}

// symlinkInside reports whether link resolves to a directory inside root other than root/synced.
func symlinkInside(root, link string) bool {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(realRoot, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if rel == "synced" || strings.HasPrefix(rel, "synced"+string(filepath.Separator)) {
		return false
	}
	info, err := os.Stat(target)
	return err == nil && info.IsDir()
}

// loadSource reads and parses one agent or skill definition file.
func loadSource(path, dirName string) (Source, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return Source{}, fmt.Errorf("read %s: %w", path, err)
	}
	fm, body := textutil.ParseFrontmatter(string(text))
	return Source{dirName: dirName, frontmatter: fm, body: body}, nil
}

// readTextNoFollow reads a file, refusing to follow a symlink at the final component (TOCTOU guard).
func readTextNoFollow(path string) (string, bool) {
	f, err := paths.OpenNoFollow(path, os.O_RDONLY, 0)
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
