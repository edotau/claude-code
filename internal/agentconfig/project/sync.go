package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Options configures a sync run; Check implies DryRun and exits 1 on any pending write, link or prune.
type Options struct {
	DryRun  bool
	NoPrune bool
	Check   bool
	Agent   string    // one of Names(); "" = every agent
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
	synced, skipped, errored, pending, current, pruned int
}

func (r *syncResult) record(w writeResult) {
	switch w {
	case written:
		r.synced++
		r.pending++
	case unchanged:
		r.synced++
		r.current++
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
	r.current += o.current
	r.pruned += o.pruned
}

// Sync projects agents/ and skills/ into every agent home and prunes stale output. Exit 0 ok, 1 error or stale.
func Sync(opts Options) int {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	dryRun := opts.DryRun || opts.Check
	home, _ := os.UserHomeDir()
	root := paths.ConfigDir()
	if home == "" {
		fmt.Fprintln(os.Stderr, "sync: HOME is not set")
		return 1
	}
	names := Names()
	if opts.Agent != "" {
		if _, ok := targets[opts.Agent]; !ok {
			fmt.Fprintf(os.Stderr, "sync: unknown vendor %q (want one of %s)\n", opts.Agent, strings.Join(names, ", "))
			return 2
		}
		names = []string{opts.Agent}
	}
	agentsDir, skillsDir := filepath.Join(root, "agents"), filepath.Join(root, "skills")
	if !paths.IsDir(agentsDir) {
		fmt.Fprintf(os.Stderr, "sync: agents directory not found: %s\n", agentsDir)
		return 1
	}
	agents, err := DiscoverAgents(agentsDir)
	if err != nil {
		return syncFail(err)
	}
	skills, err := DiscoverSkills(skillsDir)
	if err != nil {
		return syncFail(err)
	}
	if err := validateUniqueNames(skills); err != nil {
		return syncFail(err)
	}
	prune := !opts.NoPrune && len(agents)+len(skills) > 0
	if !opts.NoPrune && !prune {
		fmt.Fprintln(os.Stderr, "sync: no agents/skills discovered; refusing to prune")
	}

	docs, hasDocs, err := renderDocs(root, agents)
	if err != nil {
		return syncFail(err)
	}
	var res syncResult
	for _, pname := range names {
		res.add(syncProjectionAssets(out, pname, root, home, skillsDir, agents, skills, docs, hasDocs, dryRun, prune))
	}
	fmt.Fprintln(out, "== skills ==")
	agentsSkills := AgentsSkillsDir(home)
	res.add(syncSkillLinks(out, skills, skillsDir, agentsSkills, prune, dryRun))
	if prune {
		res.add(pruneLegacyGeminiSkills(out, LegacyGeminiSkillsDir(home), dryRun))
	}
	return finishSync(out, opts, dryRun, res)
}

// syncFail reports a fatal sync error on stderr and returns the exit code.
func syncFail(err error) int {
	fmt.Fprintln(os.Stderr, "sync:", err)
	return 1
}

// syncSkillLinks links every skill into dir and, when prune is set, removes links the roster no longer projects.
func syncSkillLinks(out io.Writer, skills []Source, skillsDir, dir string, prune, dryRun bool) syncResult {
	res := linkSkills(out, skills, skillsDir, dir, dryRun)
	if prune {
		res.add(pruneSkillLinks(out, dir, skillsDir, projectedSkillNames(skills), dryRun))
	}
	return res
}

// mkdirUnlessDry creates dirs unless this is a dry run.
func mkdirUnlessDry(dryRun bool, dirs ...string) {
	if dryRun {
		return
	}
	for _, d := range dirs {
		_ = os.MkdirAll(d, 0o755)
	}
}

// finishSync prints the run summary and maps the totals onto Sync's exit code.
func finishSync(out io.Writer, opts Options, dryRun bool, res syncResult) int {
	pruned := res.pruned

	summary := fmt.Sprintf("\nDone: %d synced, %d skipped", res.synced, res.skipped)
	if dryRun {
		summary = fmt.Sprintf("\nDone: %d to sync, %d up to date, %d skipped", res.pending, res.current, res.skipped)
	}
	if pruned > 0 {
		verb := "pruned"
		if dryRun {
			verb = "to prune"
		}
		summary += fmt.Sprintf(", %d %s", pruned, verb)
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

func syncProjectionAssets(out io.Writer, name, root, home, skillsDir string, agents, skills []Source, docs string, hasDocs, dryRun, prune bool) syncResult {
	fmt.Fprintf(out, "== %s ==\n", name)
	tg := targets[name]
	res := syncProjection(out, name, tg, agents, Dir(root, name), tg.home(home), dryRun, prune)
	if tg.skills != nil {
		// agy reads its own skills dir rather than ~/.agents/skills.
		res.add(syncSkillLinks(out, skills, skillsDir, tg.skills(home), prune, dryRun))
	}
	if rel, ok := instructionFiles[name]; ok && hasDocs {
		mkdirUnlessDry(dryRun, filepath.Dir(filepath.Join(home, rel)), Root(root, name))
		res.record(writeProjection(out, filepath.Join(Root(root, name), filepath.Base(rel)), docs, dryRun))
		res.record(writeOwnedFile(out, filepath.Join(home, rel), docs, dryRun))
	}
	return res
}

// syncProjection renders each agent into distDir and homeDir, then prunes generated files the roster dropped.
func syncProjection(out io.Writer, target string, t projectionTarget, agents []Source, distDir, homeDir string, dryRun, prune bool) syncResult {
	var res syncResult
	rendered := make(map[string]bool, len(agents))
	for _, agent := range agents {
		name := agent.Name()
		if name == target {
			continue // never project a target's own agent into its own CLI home (nesting hazard)
		}
		if !paths.SafeName(name) {
			fmt.Fprintf(os.Stderr, "  ERROR unsafe agent name %q (must be a plain path component)\n", name)
			res.errored++
			continue
		}
		file := name + t.ext
		content := t.render(agent)
		rendered[file] = true
		mkdirUnlessDry(dryRun, distDir, homeDir)
		res.record(writeProjection(out, filepath.Join(distDir, file), content, dryRun))
		res.record(writeOwnedFile(out, filepath.Join(homeDir, file), content, dryRun))
	}
	if !prune {
		return res
	}
	res.add(pruneGeneratedFiles(out, distDir, t.ext, rendered, dryRun))
	res.add(pruneGeneratedFiles(out, homeDir, t.ext, rendered, dryRun))
	return res
}

// writeOwnedFile writes an agent-home copy, keeping any symlink or bannerless (hand-authored) file at that path.
func writeOwnedFile(out io.Writer, path, content string, dryRun bool) writeResult {
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		fmt.Fprintf(out, "  KEEP  %s (hand-authored symlink)\n", paths.ShortHome(path))
		return kept
	case err == nil && !isGeneratedFile(path):
		fmt.Fprintf(out, "  KEEP  %s (hand-authored; remove it to let the harness own it)\n", paths.ShortHome(path))
		return kept
	}
	return writeProjection(out, path, content, dryRun)
}

// writeProjection compares against disk (so --check fires only on real change), writes on change, reports.
func writeProjection(out io.Writer, outputFile, content string, dryRun bool) writeResult {
	rel := paths.ShortHome(outputFile)
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
