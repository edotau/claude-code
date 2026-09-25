package crossgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// isGeneratedSkillDir reports whether path is a real dir (not a symlink) whose SKILL.md carries a generated banner.
func isGeneratedSkillDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	skillFile := filepath.Join(path, "SKILL.md")
	if fi, err := os.Stat(skillFile); err != nil || fi.IsDir() {
		return false
	}
	text, ok := readTextNoFollow(skillFile)
	if !ok {
		return false
	}
	return hasGeneratedBanner(text)
}

// hasGeneratedBanner reports whether a banner is the first non-empty line after the closing frontmatter
// delimiter (or the first line with no frontmatter) — the exact shape buildFrontmatterFile writes.
func hasGeneratedBanner(text string) bool {
	body := text
	if m := fmRes().split.FindStringSubmatch(text); m != nil {
		body = m[2]
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, prefix := range generatedBannerPrefixes {
			if strings.HasPrefix(line, prefix) {
				return true
			}
		}
		return false
	}
	return false
}

// removeGeneratedSkillDir deletes a generated skill dir: its symlinks, then SKILL.md, then the dir itself.
// Never RemoveAll; refuses any path that is not a direct child of root.
func removeGeneratedSkillDir(path, root string) error {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if filepath.Dir(resolved) != absRoot {
		return fmt.Errorf("refusing to delete dir outside %s: %s", root, path)
	}
	// Symlinks only (os.Remove never follows one); before SKILL.md so a failed delete still reads as generated.
	entries, _ := os.ReadDir(path)
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if err := os.Remove(filepath.Join(path, e.Name())); err != nil {
			return err
		}
	}
	skillFile := filepath.Join(path, "SKILL.md")
	if _, err := os.Stat(skillFile); err == nil {
		if err := os.Remove(skillFile); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// pruneOrphans removes banner-marked dirs under root whose name is not in projected; returns the count.
// A dir whose SOURCE still exists (skip-listed) is reported as not-projected, not orphaned.
func pruneOrphans(out io.Writer, agents, skills []Source, projected map[string]bool, root string, dryRun bool) int {
	skipOutputs := make(map[string]bool, len(agents)+len(skills))
	for _, s := range skills {
		if skipNames[s.dirName] {
			skipOutputs[s.Name()] = true
		}
	}
	for _, a := range agents {
		if skipNames[a.dirName] {
			skipOutputs[a.Name()] = true
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	removed := 0
	for _, n := range names {
		entry := filepath.Join(root, n)
		if projected[n] || !isGeneratedSkillDir(entry) {
			continue
		}
		reason := fmt.Sprintf("orphaned: no source '%s'", n) // a name a skill owns is projected, so it is rewritten, not pruned
		if skipOutputs[n] {
			reason = fmt.Sprintf("not projected to gemini: '%s' is Claude-Code-only", n)
		}
		if !dryRun {
			if err := removeGeneratedSkillDir(entry, root); err != nil {
				fmt.Fprintf(os.Stderr, "  prune error: %v\n", err)
				continue
			}
		}
		fmt.Fprintf(out, "  PRUNE %s (%s)\n", displayPath(entry), reason)
		removed++
	}
	return removed
}
