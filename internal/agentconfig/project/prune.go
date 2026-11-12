// Package project projects agents/ into every agent's native subagent dir (rendered under state/projections),
// links skills/ into ~/.agents/skills (plus agy's and copilot's own skills dirs), and renders each agent's
// global instruction file (~/.codex/AGENTS.md, ~/.gemini/GEMINI.md, ~/.copilot/copilot-instructions.md).
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

// isGeneratedFile reports whether path is a regular file (not a symlink) whose first body line is a sync banner.
func isGeneratedFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	text, ok := readTextNoFollow(path)
	return ok && hasGeneratedBanner(text)
}

// hasGeneratedBanner reports whether a banner is the first non-empty body line after any frontmatter.
func hasGeneratedBanner(text string) bool {
	_, body := textutil.ParseFrontmatter(text)
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

// pruneEntries removes each entry of dir that match accepts (with the PRUNE reason), counting removals and failures.
func pruneEntries(out io.Writer, dir string, match func(os.DirEntry) (string, bool), dryRun bool) syncResult {
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
			if err := os.Remove(path); err != nil {
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
	return pruneEntries(out, dir, match, dryRun)
}
