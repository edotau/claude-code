package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
)

// ensureLink makes link → target: re-points a stale symlink under ownedRoot, keeps any other existing entry.
func ensureLink(out io.Writer, link, target, ownedRoot string, dryRun bool) writeResult {
	rel := paths.ShortHome(link)
	info, err := os.Lstat(link)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		dest, _ := os.Readlink(link)
		if dest == target {
			status(out, "OK", rel, "linked")
			return unchanged
		}
		if !paths.Within(dest, ownedRoot) {
			status(out, "KEEP", rel, "hand-authored symlink")
			return kept
		}
		if !dryRun {
			if rerr := os.Remove(link); rerr != nil {
				fmt.Fprintf(os.Stderr, "  ERROR %s: %v\n", rel, rerr)
				return failed
			}
		}
	case err == nil:
		status(out, "KEEP", rel, "hand-authored skill; remove it to let the harness link its copy")
		return kept
	}
	if dryRun {
		linkStatus(out, "DRY", rel, paths.ShortHome(target))
		return written
	}
	if serr := os.Symlink(target, link); serr != nil {
		fmt.Fprintf(os.Stderr, "  ERROR %s: %v\n", rel, serr)
		return failed
	}
	linkStatus(out, "LINK", rel, paths.ShortHome(target))
	return written
}

// syncedDir holds claude.ai account skills; no agent gets them, and a link into it is never pruned.
const syncedDir = "synced"

// pruneSkillLinks removes unprojected symlinks in agentsSkills that point under skillsDir; all else is untouched.
func pruneSkillLinks(out io.Writer, agentsSkills, skillsDir string, projected map[string]bool, dryRun bool) syncResult {
	syncedPath := filepath.Join(skillsDir, syncedDir)
	match := func(e os.DirEntry) (string, bool) {
		if e.Type()&os.ModeSymlink == 0 || projected[e.Name()] {
			return "", false
		}
		dest, err := os.Readlink(filepath.Join(agentsSkills, e.Name()))
		if err != nil || !paths.Within(dest, skillsDir) {
			return "", false
		}
		if paths.Within(dest, syncedPath) {
			status(out, "EXTRA", paths.ShortHome(filepath.Join(agentsSkills, e.Name())), "synced skills go to no agent; preserved")
			return "", false
		}
		return "orphaned link -> " + paths.ShortHome(dest), true
	}
	return pruneEntries(out, agentsSkills, match, os.Remove, dryRun)
}

// pruneLegacyGeminiSkills removes banner-marked skill dirs under ~/.gemini/skills; remove after 2026-11-13.
func pruneLegacyGeminiSkills(out io.Writer, root string, dryRun bool) syncResult {
	match := func(e os.DirEntry) (string, bool) {
		return "legacy projection; skills now link via ~/.agents/skills", isGeneratedSkillDir(filepath.Join(root, e.Name()))
	}
	remove := func(path string) error { return removeGeneratedSkillDir(path, root) }
	return pruneEntries(out, root, match, remove, dryRun)
}
