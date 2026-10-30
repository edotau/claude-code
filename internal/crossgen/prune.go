package crossgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jeduden/mdsmith/pkg/markdown"
)

// isGeneratedFile reports whether path is a regular file (not a symlink) whose first body line is a sync banner.
func isGeneratedFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	text, ok := readTextNoFollow(path)
	return ok && hasGeneratedBanner(text)
}

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
	return ok && hasGeneratedBanner(text)
}

// hasGeneratedBanner reports whether a banner is the first non-empty line after the closing frontmatter
// delimiter (or the first line with no frontmatter) — the exact shape every renderer writes.
func hasGeneratedBanner(text string) bool {
	body := text
	if prefix, rest := markdown.StripFrontMatter([]byte(text)); prefix != nil {
		body = string(rest)
	}
	for line := range strings.SplitSeq(body, "\n") {
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

// pruneGeneratedFiles removes banner-marked <name><ext> files in dir not in keep; anything else is left alone.
func pruneGeneratedFiles(out io.Writer, dir, ext string, keep map[string]bool, dryRun bool) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	pruned := 0
	for _, e := range sortedEntries(entries) {
		path := filepath.Join(dir, e.Name())
		if keep[e.Name()] || !strings.HasSuffix(e.Name(), ext) || !isGeneratedFile(path) {
			continue
		}
		if !dryRun {
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "  prune error: %v\n", err)
				continue
			}
		}
		fmt.Fprintf(out, "  PRUNE %s (orphaned: no agent source)\n", displayPath(path))
		pruned++
	}
	return pruned
}

// removeGeneratedSkillDir deletes an old generated skill dir: its symlinks, then SKILL.md, then the dir itself.
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
