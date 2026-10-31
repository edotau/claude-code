package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// linkSkills points ~/.agents/skills/<name> at <skillsDir>/<dir> for every projected skill: one location, read
// natively by every agent, and the source is already git-tracked. Returns the count of pending link changes.
func linkSkills(out io.Writer, skills []Source, skillsDir, agentsSkills string, dryRun bool) (res syncResult) {
	if !dryRun {
		_ = os.MkdirAll(agentsSkills, 0o755)
	}
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

// ensureLink makes link → target. A symlink already pointing elsewhere under ownedRoot is re-pointed (ours, stale);
// any other existing entry — a real dir, or a symlink into foreign territory — is hand-authored and kept.
func ensureLink(out io.Writer, link, target, ownedRoot string, dryRun bool) writeResult {
	rel := displayPath(link)
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
		fmt.Fprintf(out, "  DRY   %s -> %s\n", rel, displayPath(target))
		return written
	}
	if serr := os.Symlink(target, link); serr != nil {
		fmt.Fprintf(os.Stderr, "  ERROR %s: %v\n", rel, serr)
		return failed
	}
	fmt.Fprintf(out, "  LINK  %s -> %s\n", rel, displayPath(target))
	return written
}

// pruneSkillLinks removes symlinks in agentsSkills that point under skillsDir but whose name is no longer projected.
// Real dirs and symlinks into any other tree are never touched.
func pruneSkillLinks(out io.Writer, agentsSkills, skillsDir string, projected map[string]bool, dryRun bool) int {
	entries, err := os.ReadDir(agentsSkills)
	if err != nil {
		return 0
	}
	pruned := 0
	for _, e := range sortedEntries(entries) {
		if e.Type()&os.ModeSymlink == 0 || projected[e.Name()] {
			continue
		}
		link := filepath.Join(agentsSkills, e.Name())
		dest, rerr := os.Readlink(link)
		if rerr != nil || !under(dest, skillsDir) {
			continue
		}
		if !dryRun {
			if err := os.Remove(link); err != nil {
				fmt.Fprintf(os.Stderr, "  prune error: %v\n", err)
				continue
			}
		}
		fmt.Fprintf(out, "  PRUNE %s (orphaned link -> %s)\n", displayPath(link), displayPath(dest))
		pruned++
	}
	return pruned
}

// pruneLegacyGeminiSkills removes every banner-marked skill dir an earlier sync wrote under ~/.gemini/skills, the
// pre-agy location: skills now reach agy through GeminiSkillsDir, and a copy in both tiers conflicts.
// Migration: remove after 2026-11-13 once every machine has run a post-2026-10-17 build.
func pruneLegacyGeminiSkills(out io.Writer, root string, dryRun bool) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	pruned := 0
	for _, e := range sortedEntries(entries) {
		entry := filepath.Join(root, e.Name())
		if !isGeneratedSkillDir(entry) {
			continue
		}
		if !dryRun {
			if err := removeGeneratedSkillDir(entry, root); err != nil {
				fmt.Fprintf(os.Stderr, "  prune error: %v\n", err)
				continue
			}
		}
		fmt.Fprintf(out, "  PRUNE %s (legacy projection; skills now link via ~/.agents/skills)\n", displayPath(entry))
		pruned++
	}
	return pruned
}

// under reports whether path is root or lies beneath it (lexically; both absolute).
func under(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sortedEntries(entries []os.DirEntry) []os.DirEntry {
	sorted := append([]os.DirEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name() < sorted[j].Name() })
	return sorted
}
