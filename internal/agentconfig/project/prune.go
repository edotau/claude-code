// Package project projects agents/ into every agent's native subagent dir (tracked under dist/), links skills/
// into ~/.agents/skills (plus agy's own GeminiSkillsDir), and renders each agent's global instruction file
// (~/.codex/AGENTS.md, ~/.gemini/GEMINI.md).
package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/jeduden/mdsmith/pkg/markdown"
)

// isGeneratedFile reports whether path is a regular file (not a symlink) whose first body line is a sync banner.
func isGeneratedFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return isGeneratedText(path)
}

// isGeneratedText reports whether path reads (without following a symlink) as text carrying a generated banner.
func isGeneratedText(path string) bool {
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
	return isGeneratedText(skillFile)
}

// hasGeneratedBanner reports whether a banner is the first non-empty body line after any frontmatter.
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

// pruneEntries removes every entry of dir that match accepts (returning the PRUNE reason), counting the removals.
func pruneEntries(out io.Writer, dir string, match func(os.DirEntry) (string, bool), remove func(string) error, dryRun bool) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	pruned := 0
	for _, e := range entries {
		reason, ok := match(e)
		if !ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !dryRun {
			if err := remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "  prune error: %v\n", err)
				continue
			}
		}
		fmt.Fprintf(out, "  PRUNE %s (%s)\n", paths.ShortHome(path), reason)
		pruned++
	}
	return pruned
}

// pruneGeneratedFiles removes banner-marked <name><ext> files in dir not in keep; anything else is left alone.
func pruneGeneratedFiles(out io.Writer, dir, ext string, keep map[string]bool, dryRun bool) int {
	match := func(e os.DirEntry) (string, bool) {
		ok := !keep[e.Name()] && strings.HasSuffix(e.Name(), ext) && isGeneratedFile(filepath.Join(dir, e.Name()))
		return "orphaned: no agent source", ok
	}
	return pruneEntries(out, dir, match, os.Remove, dryRun)
}

// removeGeneratedSkillDir deletes a generated skill dir (symlinks, SKILL.md, dir); direct children of root only.
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
