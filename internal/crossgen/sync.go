package crossgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// SyncOptions configures a sync run; Check implies DryRun and exits 1 on any pending write or prune.
type SyncOptions struct {
	DryRun  bool
	NoPrune bool
	Check   bool
	Out     io.Writer // status lines; nil → os.Stdout
}

// writeResult classifies one projection write.
type writeResult int

const (
	written   writeResult = iota // written, or would be in dry-run: pending work for --check
	unchanged                    // already current
	kept                         // hand-authored entry preserved
	failed
)

// syncResult counts one run's writes; pending (not synced) is what --check gates on.
type syncResult struct {
	synced, skipped, errored, pending int
}

func (r *syncResult) record(w writeResult) {
	switch w {
	case written:
		r.synced++
		r.pending++
	case unchanged:
		r.synced++
	case kept:
		r.skipped++
	default:
		r.errored++
	}
}

// Sync projects agents/ and skills/ under paths.ConfigDir() into GeminiSkillsDir(), then prunes orphans.
// Exit code: 0 ok, 1 write error or stale under --check.
func Sync(opts SyncOptions) int {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	dryRun := opts.DryRun || opts.Check
	root := paths.ConfigDir()
	agentsDir, skillsDir := filepath.Join(root, "agents"), filepath.Join(root, "skills")
	outRoot := GeminiSkillsDir()
	if outRoot == "" {
		fmt.Fprintln(os.Stderr, "sync: HOME is not set")
		return 1
	}
	if !isDir(agentsDir) {
		fmt.Fprintf(os.Stderr, "sync: agents directory not found: %s\n", agentsDir)
		return 1
	}
	agents, err := DiscoverAgents(agentsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync:", err)
		return 1
	}
	skills := DiscoverSkills(skillsDir)
	for _, v := range []struct {
		kind string
		srcs []Source
	}{{"agent", agents}, {"skill", skills}} {
		if err := validateUniqueNames(v.kind, v.srcs); err != nil {
			fmt.Fprintln(os.Stderr, "sync:", err)
			return 1
		}
	}

	projected := projectedNames(agents, skills)
	res := syncGemini(out, agents, skills, skillsDir, outRoot, dryRun)
	pruned := runPrune(out, agents, skills, projected, outRoot, dryRun, opts.NoPrune)

	verb := ""
	if dryRun {
		verb = "would be "
	}
	summary := fmt.Sprintf("\nDone: %d %ssynced, %d skipped", res.synced, verb, res.skipped)
	if pruned > 0 {
		summary += fmt.Sprintf(", %d %spruned", pruned, verb)
	}
	fmt.Fprintln(out, summary)
	if res.errored > 0 {
		fmt.Fprintf(os.Stderr, "sync: %d file(s) failed to write — projection is incomplete\n", res.errored)
		return 1
	}
	if opts.Check && (res.pending > 0 || pruned > 0) {
		fmt.Fprintf(os.Stderr, "projection is stale (%d to write, %d to prune) — run `claude-code agents sync --gemini`\n",
			res.pending, pruned)
		return 1
	}
	return 0
}

// runPrune prunes orphaned projections unless disabled or nothing was discovered; returns the count pruned.
func runPrune(out io.Writer, agents, skills []Source, projected map[string]bool, outRoot string, dryRun, noPrune bool) int {
	if noPrune {
		return 0
	}
	fmt.Fprintln(out, "\n== prune ==")
	if len(agents)+len(skills) == 0 {
		fmt.Fprintln(os.Stderr, "  SKIP — no agents/skills discovered; refusing to prune")
		return 0
	}
	pruned := pruneOrphans(out, agents, skills, projected, outRoot, dryRun)
	if pruned == 0 {
		fmt.Fprintln(out, "  (no orphans)")
	}
	return pruned
}

// syncGemini writes agent projections then real skills; the skip rules mirror projectedNames exactly.
func syncGemini(out io.Writer, agents, skills []Source, skillsDir, outRoot string, dryRun bool) syncResult {
	if !dryRun {
		_ = os.MkdirAll(outRoot, 0o755)
	}
	var res syncResult
	realSkill := realSkillNames(skills)
	for _, agent := range agents {
		if skipNames[agent.dirName] {
			fmt.Fprintf(out, "  SKIP  %s (Claude-Code-only)\n", agent.dirName)
			res.skipped++
			continue
		}
		name, content := transformAgentSkill(agent)
		if realSkill[name] {
			fmt.Fprintf(out, "  SKIP  %s (name owned by a real skill)\n", name)
			res.skipped++
			continue
		}
		res.record(writeSkillDir(out, outRoot, name, content, dryRun))
	}
	for _, skill := range skills {
		if skipNames[skill.dirName] {
			fmt.Fprintf(out, "  SKIP  %s (Claude-Code-only skill)\n", skill.dirName)
			res.skipped++
			continue
		}
		name, content := transformRealSkill(skill)
		w := writeSkillDir(out, outRoot, name, content, dryRun)
		res.record(w)
		// The body cites its own scripts/ and references/, so the projection must carry them.
		if w == written || w == unchanged {
			res.pending += linkSkillAssets(out, filepath.Join(skillsDir, skill.dirName), filepath.Join(outRoot, name), dryRun)
		}
	}
	return res
}

// writeSkillDir writes <outRoot>/<name>/SKILL.md, keeping any symlink entry or bannerless (hand-authored) dir.
func writeSkillDir(out io.Writer, outRoot, name, content string, dryRun bool) writeResult {
	if !safeName(name) {
		fmt.Fprintf(os.Stderr, "  ERROR unsafe skill name %q (must be a plain path component)\n", name)
		return failed
	}
	outputDir := filepath.Join(outRoot, name)
	info, err := os.Lstat(outputDir)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		fmt.Fprintf(out, "  KEEP  %s (hand-authored symlink)\n", displayPath(outputDir))
		return kept
	case err == nil && !isGeneratedSkillDir(outputDir):
		fmt.Fprintf(out, "  KEEP  %s (hand-authored skill)\n", displayPath(outputDir))
		return kept
	}
	return writeProjection(out, filepath.Join(outputDir, "SKILL.md"), content, dryRun)
}

// writeProjection compares against disk (so --check fires only on real change), writes on change, reports.
func writeProjection(out io.Writer, outputFile, content string, dryRun bool) writeResult {
	rel := displayPath(outputFile)
	if dryRun {
		if existing, err := os.ReadFile(outputFile); err == nil && string(existing) == content {
			fmt.Fprintf(out, "  OK    %s (unchanged)\n", rel)
			return unchanged
		}
		fmt.Fprintf(out, "  DRY   %s\n", rel)
		return written
	}
	wrote, err := paths.WriteIfChanged(outputFile, []byte(content), 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR %s: %v\n", rel, err)
		return failed
	}
	if !wrote {
		fmt.Fprintf(out, "  OK    %s (unchanged)\n", rel)
		return unchanged
	}
	fmt.Fprintf(out, "  WRITE %s\n", rel)
	return written
}

// linkSkillAssets symlinks source entries (except SKILL.md, dotfiles, __pycache__) into the projection dir.
func linkSkillAssets(out io.Writer, srcDir, outDir string, dryRun bool) int {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return 0
	}
	stale := 0
	for _, e := range entries {
		if e.Name() == "SKILL.md" || strings.HasPrefix(e.Name(), ".") || e.Name() == "__pycache__" {
			continue
		}
		target := filepath.Join(srcDir, e.Name())
		link := filepath.Join(outDir, e.Name())
		switch info, lerr := os.Lstat(link); {
		case lerr == nil && info.Mode()&os.ModeSymlink != 0:
			if dest, rerr := os.Readlink(link); rerr == nil && dest == target {
				continue
			}
			if !dryRun {
				if rerr := os.Remove(link); rerr != nil {
					fmt.Fprintf(os.Stderr, "  ERROR %s: %v\n", displayPath(link), rerr)
					continue
				}
			}
		case lerr == nil:
			fmt.Fprintf(out, "  KEEP  %s (hand-authored asset)\n", displayPath(link))
			continue
		}
		stale++
		if dryRun {
			fmt.Fprintf(out, "  DRY   %s -> %s\n", displayPath(link), displayPath(target))
			continue
		}
		if serr := os.Symlink(target, link); serr != nil {
			fmt.Fprintf(os.Stderr, "  ERROR %s: %v\n", displayPath(link), serr)
			stale--
			continue
		}
		fmt.Fprintf(out, "  LINK  %s -> %s\n", displayPath(link), displayPath(target))
	}
	return stale
}
