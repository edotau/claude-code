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
	text, ok := readTextNoFollow(path)
	return ok && hasGeneratedBanner(text)
}

// isGeneratedSkillDir reports whether path is a real dir (not a symlink) whose SKILL.md carries a generated banner.
func isGeneratedSkillDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	return isGeneratedFile(filepath.Join(path, "SKILL.md"))
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

// pruneEntries removes every entry of dir that match accepts (returning the PRUNE reason), counting removals and failures.
func pruneEntries(out io.Writer, dir string, match func(os.DirEntry) (string, bool), remove func(string) error, dryRun bool) syncResult {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return syncResult{}
		}
		fmt.Fprintf(os.Stderr, "  prune error: %v\n", err)
		return syncResult{errored: 1}
	}
	var res syncResult
	for _, e := range entries {
		reason, ok := match(e)
		if !ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !dryRun {
			if err := remove(path); err != nil {
				fmt.Fprintf(os.Stderr, "  prune error: %v\n", err)
				res.errored++
				continue
			}
		}
		status(out, "PRUNE", paths.ShortHome(path), reason)
		res.pruned++
	}
	return res
}

// pruneGeneratedFiles removes banner-marked <name><ext> files in dir not in keep; anything else is left alone.
func pruneGeneratedFiles(out io.Writer, dir, ext string, keep map[string]bool, dryRun bool) syncResult {
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
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	// Check before deleting anything: a hand-added entry would fail the final rmdir after SKILL.md was gone.
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 && e.Name() != "SKILL.md" {
			return fmt.Errorf("refusing to delete %s: holds %s", path, e.Name())
		}
	}
	// Symlinks, then the SKILL.md marker, then the dir: the marker survives any failure before the last step.
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if err := os.Remove(filepath.Join(path, e.Name())); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(path, "SKILL.md")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Remove(path)
}
