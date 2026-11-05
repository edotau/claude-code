package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// linkSkills points <agentsSkills>/<name> at <skillsDir>/<dir> for every projected skill and counts the outcomes.
func linkSkills(out io.Writer, skills []Source, skillsDir, agentsSkills string, dryRun bool) (res syncResult) {
	mkdirUnlessDry(dryRun, agentsSkills)
	for _, skill := range skills {
		name := skill.Name()
		if !paths.SafeName(name) {
			fmt.Fprintf(os.Stderr, "  ERROR unsafe skill name %q (must be a plain path component)\n", name)
			res.errored++
			continue
		}
		res.record(ensureLink(out, filepath.Join(agentsSkills, name), filepath.Join(skillsDir, skill.dirName), skillsDir, dryRun))
	}
	return res
}

// ensureLink makes link → target: re-points a stale symlink under ownedRoot, keeps any other existing entry.
func ensureLink(out io.Writer, link, target, ownedRoot string, dryRun bool) writeResult {
	rel := paths.ShortHome(link)
	info, err := os.Lstat(link)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		dest, _ := os.Readlink(link)
		if dest == target {
			fmt.Fprintf(out, "  OK    %s (linked)\n", rel)
			return unchanged
		}
		if !under(dest, ownedRoot) {
			fmt.Fprintf(out, "  KEEP  %s (hand-authored symlink)\n", rel)
			return kept
		}
		if !dryRun {
			if rerr := os.Remove(link); rerr != nil {
				fmt.Fprintf(os.Stderr, "  ERROR %s: %v\n", rel, rerr)
				return failed
			}
		}
	case err == nil:
		fmt.Fprintf(out, "  KEEP  %s (hand-authored skill; remove it to let the harness link its copy)\n", rel)
		return kept
	}
	if dryRun {
		fmt.Fprintf(out, "  DRY   %s -> %s\n", rel, paths.ShortHome(target))
		return written
	}
	if serr := os.Symlink(target, link); serr != nil {
		fmt.Fprintf(os.Stderr, "  ERROR %s: %v\n", rel, serr)
		return failed
	}
	fmt.Fprintf(out, "  LINK  %s -> %s\n", rel, paths.ShortHome(target))
	return written
}

// syncedDir holds claude.ai account skills; no agent gets them, and a link into it is never pruned.
const syncedDir = "synced"

// pruneSkillLinks removes unprojected symlinks in agentsSkills that point under skillsDir; all else is untouched.
func pruneSkillLinks(out io.Writer, agentsSkills, skillsDir string, projected map[string]bool, dryRun bool) syncResult {
	match := func(e os.DirEntry) (string, bool) {
		if e.Type()&os.ModeSymlink == 0 || projected[e.Name()] {
			return "", false
		}
		dest, err := os.Readlink(filepath.Join(agentsSkills, e.Name()))
		if err != nil || !under(dest, skillsDir) {
			return "", false
		}
		if under(dest, filepath.Join(skillsDir, syncedDir)) {
			fmt.Fprintf(out, "  EXTRA %s (synced skills go to no agent; preserved)\n", paths.ShortHome(filepath.Join(agentsSkills, e.Name())))
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

// under reports whether path is root or lies beneath it (lexically; both absolute).
func under(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
