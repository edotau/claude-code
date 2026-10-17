package crossgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// SyncOptions configures a sync run; Check implies DryRun and exits 1 on any pending write, link or prune.
type SyncOptions struct {
	DryRun  bool
	NoPrune bool
	Check   bool
	Vendor  string    // one of VendorNames(); "" = every vendor
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

func (r *syncResult) add(o syncResult) {
	r.synced += o.synced
	r.skipped += o.skipped
	r.errored += o.errored
	r.pending += o.pending
}

// Sync renders agents/ into the tracked dist/<vendor>/agents tree, installs each vendor's copy into its home,
// links skills/ into ~/.agents/skills, and prunes what the roster no longer owns. Exit 0 ok, 1 error or stale (--check).
func Sync(opts SyncOptions) int {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	dryRun := opts.DryRun || opts.Check
	root, home := paths.ConfigDir(), userHome()
	if home == "" {
		fmt.Fprintln(os.Stderr, "sync: HOME is not set")
		return 1
	}
	vendors := VendorNames()
	if opts.Vendor != "" {
		if _, ok := targets[opts.Vendor]; !ok {
			fmt.Fprintf(os.Stderr, "sync: unknown vendor %q (want one of %s)\n", opts.Vendor, strings.Join(vendors, ", "))
			return 2
		}
		vendors = []string{opts.Vendor}
	}
	agentsDir, skillsDir := filepath.Join(root, "agents"), filepath.Join(root, "skills")
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
	prune := !opts.NoPrune && len(agents)+len(skills) > 0
	if !opts.NoPrune && !prune {
		fmt.Fprintln(os.Stderr, "sync: no agents/skills discovered; refusing to prune")
	}

	docs, hasDocs, err := renderDocs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync:", err)
		return 1
	}
	var res syncResult
	pruned := 0
	for _, vendor := range vendors {
		fmt.Fprintf(out, "== %s ==\n", vendor)
		r, p := syncVendor(out, targets[vendor], agents, DistDir(root, vendor), targets[vendor].home(home), dryRun, prune)
		res.add(r)
		pruned += p
		if rel, ok := instructionFiles[vendor]; ok && hasDocs {
			if !dryRun {
				_ = os.MkdirAll(filepath.Dir(filepath.Join(home, rel)), 0o755)
				_ = os.MkdirAll(filepath.Join(root, "dist", vendor), 0o755)
			}
			res.record(writeProjection(out, filepath.Join(root, "dist", vendor, filepath.Base(rel)), docs, dryRun))
			res.record(writeOwnedFile(out, filepath.Join(home, rel), docs, dryRun))
		}
	}
	fmt.Fprintln(out, "== skills ==")
	agentsSkills := AgentsSkillsDir(home)
	res.add(linkSkills(out, skills, skillsDir, agentsSkills, dryRun))
	if prune {
		pruned += pruneSkillLinks(out, agentsSkills, skillsDir, projectedSkillNames(skills), dryRun)
		pruned += pruneLegacyGeminiSkills(out, LegacyGeminiSkillsDir(home), dryRun)
	}

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
		fmt.Fprintf(os.Stderr, "sync: %d file(s) failed — projection is incomplete\n", res.errored)
		return 1
	}
	if opts.Check && (res.pending > 0 || pruned > 0) {
		fmt.Fprintf(os.Stderr, "projection is stale (%d to write, %d to prune) — run `claude-code agents sync`\n", res.pending, pruned)
		return 1
	}
	return 0
}

// syncVendor renders every projected agent into distDir (tracked) and installs a copy into homeDir, then prunes
// generated files in both that the roster no longer produces.
func syncVendor(out io.Writer, t vendorTarget, agents []Source, distDir, homeDir string, dryRun, prune bool) (syncResult, int) {
	var res syncResult
	rendered := make(map[string]bool, len(agents))
	for _, agent := range agents {
		if skipNames[agent.dirName] {
			fmt.Fprintf(out, "  SKIP  %s (Claude-Code-only)\n", agent.dirName)
			res.skipped++
			continue
		}
		name := agent.Name()
		if !safeName(name) {
			fmt.Fprintf(os.Stderr, "  ERROR unsafe agent name %q (must be a plain path component)\n", name)
			res.errored++
			continue
		}
		file := name + t.ext
		content := t.render(agent)
		rendered[file] = true
		if !dryRun {
			_ = os.MkdirAll(distDir, 0o755)
			_ = os.MkdirAll(homeDir, 0o755)
		}
		res.record(writeProjection(out, filepath.Join(distDir, file), content, dryRun))
		res.record(writeOwnedFile(out, filepath.Join(homeDir, file), content, dryRun))
	}
	if !prune {
		return res, 0
	}
	pruned := pruneGeneratedFiles(out, distDir, t.ext, rendered, dryRun)
	pruned += pruneGeneratedFiles(out, homeDir, t.ext, rendered, dryRun)
	return res, pruned
}

// writeOwnedFile writes a vendor-home copy, keeping any symlink or bannerless (hand-authored) file at that path.
func writeOwnedFile(out io.Writer, path, content string, dryRun bool) writeResult {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		fmt.Fprintf(out, "  KEEP  %s (hand-authored symlink)\n", displayPath(path))
		return kept
	case err == nil && !isGeneratedFile(path):
		fmt.Fprintf(out, "  KEEP  %s (hand-authored; remove it to let the harness own it)\n", displayPath(path))
		return kept
	}
	return writeProjection(out, path, content, dryRun)
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
