package project

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Options configures a sync run; Check implies DryRun and exits 1 on pending work or a foreign entry on a managed name.
type Options struct {
	DryRun  bool
	NoPrune bool
	Check   bool
	JSON    bool      // emit per-destination rows as JSON instead of status lines
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

// syncInputs is everything Sync discovers before it writes anything.
type syncInputs struct {
	home, root, skillsDir string
	names                 []string
	agents, skills        []Source
}

// Sync projects agents/ and skills/ into every agent home and prunes stale output. Exit 0 ok, 1 error or stale.
func Sync(opts Options) int {
	if opts.JSON {
		return syncJSON(opts)
	}
	return runSync(opts)
}

func runSync(opts Options) int {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	dryRun := opts.DryRun || opts.Check
	in, code, ok := loadSyncInputs(opts)
	if !ok {
		return code
	}
	prune := !opts.NoPrune && len(in.agents)+len(in.skills) > 0
	if !opts.NoPrune && !prune {
		fmt.Fprintln(os.Stderr, "sync: "+emptySourceNote(in.skillsDir)+"; refusing to prune")
	}

	docs, hasDocs, err := renderDocs(in.root, in.agents)
	if err != nil {
		return syncFail(err)
	}
	var res syncResult
	for _, pname := range in.names {
		res.add(syncProjectionAssets(out, pname, in, docs, hasDocs, dryRun, prune))
	}
	header(out, "skills")
	agentsSkills := agentsSkillsDir(in.home)
	res.add(syncSkillLinks(out, in.skills, in.skillsDir, agentsSkills, prune, dryRun))
	if prune {
		res.add(pruneLegacyGeminiSkills(out, legacyGeminiSkillsDir(in.home), dryRun))
	}
	return finishSync(out, opts, dryRun, res)
}

// loadSyncInputs resolves HOME, the vendor roster and the discovered agents/skills; ok is false with the exit code.
func loadSyncInputs(opts Options) (in syncInputs, code int, ok bool) {
	home, _ := os.UserHomeDir()
	root := paths.ConfigDir()
	if home == "" {
		fmt.Fprintln(os.Stderr, "sync: HOME is not set")
		return in, 1, false
	}
	names := Names()
	if opts.Agent != "" {
		if _, known := targets[opts.Agent]; !known {
			fmt.Fprintf(os.Stderr, "sync: unknown agent %q (want one of %s)\n", opts.Agent, strings.Join(names, ", "))
			return in, 2, false
		}
		names = []string{opts.Agent}
	}
	agentsDir, skillsDir := filepath.Join(root, "agents"), filepath.Join(root, "skills")
	if !paths.IsDir(agentsDir) {
		fmt.Fprintf(os.Stderr, "sync: agents directory not found: %s\n", agentsDir)
		return in, 1, false
	}
	agents, err := DiscoverAgents(agentsDir)
	if err != nil {
		return in, syncFail(err), false
	}
	skills, err := discoverSkills(skillsDir)
	if err != nil {
		return in, syncFail(err), false
	}
	if err := validateUniqueNames(skills); err != nil {
		return in, syncFail(err), false
	}
	return syncInputs{home: home, root: root, skillsDir: skillsDir, names: names, agents: agents, skills: skills}, 0, true
}

// emptySourceNote tells a readable skills/ with zero skills apart from one that is missing or unreadable.
func emptySourceNote(skillsDir string) string {
	if paths.IsDir(skillsDir) {
		return "agents/ and skills/ are readable but hold zero sources"
	}
	return "skills directory not found, no agents/skills discovered"
}

// syncFail reports a fatal sync error on stderr and returns the exit code.
func syncFail(err error) int {
	fmt.Fprintln(os.Stderr, "sync:", err)
	return 1
}

// syncSkillLinks links <dir>/<name> to <skillsDir>/<dir> per projected skill; prune drops unprojected links.
func syncSkillLinks(out io.Writer, skills []Source, skillsDir, dir string, prune, dryRun bool) (res syncResult) {
	if !mkdirUnlessDry(&res, dryRun, dir) {
		return res
	}
	for _, skill := range skills {
		name := skill.Name()
		if !paths.SafeName(name) {
			fmt.Fprintf(os.Stderr, "  ERROR unsafe skill name %q (must be a plain path component)\n", name)
			res.errored++
			continue
		}
		res.record(ensureLink(out, filepath.Join(dir, name), filepath.Join(skillsDir, skill.dirName), skillsDir, dryRun))
	}
	if prune {
		res.add(pruneSkillLinks(out, dir, skillsDir, projectedSkillNames(skills), dryRun))
	}
	return res
}

// mkdirUnlessDry creates dirs unless this is a dry run; a failure is reported once, counted in res, and returns false.
func mkdirUnlessDry(res *syncResult, dryRun bool, dirs ...string) bool {
	if dryRun {
		return true
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "  ERROR %v\n", err)
			res.errored++
			return false
		}
	}
	return true
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
	if opts.Check && res.skipped > 0 {
		fmt.Fprintf(os.Stderr, "blocked: %d foreign entries on managed names; remove them, then run agents sync\n", res.skipped)
		return 1
	}
	if opts.Check && (res.pending > 0 || pruned > 0) {
		fmt.Fprintf(os.Stderr, "stale: %d to write, %d to prune; run `claude-code agents sync`\n", res.pending, pruned)
		return 1
	}
	return 0
}

func syncProjectionAssets(out io.Writer, name string, in syncInputs, docs string, hasDocs, dryRun, prune bool) syncResult {
	header(out, name)
	tg := targets[name]
	res := syncProjection(out, name, tg, in.agents, Dir(in.root, name), tg.home(in.home), dryRun, prune)
	if tg.skills != nil {
		// agy reads its own skills dir rather than ~/.agents/skills.
		res.add(syncSkillLinks(out, in.skills, in.skillsDir, tg.skills(in.home), prune, dryRun))
	}
	if rel := tg.docs; rel != "" && hasDocs && mkdirUnlessDry(&res, dryRun, filepath.Dir(filepath.Join(in.home, rel)), Root(in.root, name)) {
		res.record(writeProjection(out, filepath.Join(Root(in.root, name), filepath.Base(rel)), docs, dryRun))
		res.record(writeOwnedFile(out, filepath.Join(in.home, rel), docs, dryRun))
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
		if !mkdirUnlessDry(&res, dryRun, distDir, homeDir) {
			return res
		}
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
		status(out, "KEEP", paths.ShortHome(path), "hand-authored symlink")
		return kept
	case err == nil && !isGeneratedFile(path):
		status(out, "KEEP", paths.ShortHome(path), "hand-authored; remove it to let the harness own it")
		return kept
	}
	return writeProjection(out, path, content, dryRun)
}

// writeProjection compares against disk (so --check fires only on real change), writes on change, reports.
func writeProjection(out io.Writer, outputFile, content string, dryRun bool) writeResult {
	rel := paths.ShortHome(outputFile)
	if dryRun {
		if existing, err := os.ReadFile(outputFile); err == nil && string(existing) == content {
			status(out, "OK", rel, "unchanged")
			return unchanged
		}
		status(out, "DRY", rel, "")
		return written
	}
	wrote, err := paths.WriteIfChanged(outputFile, []byte(content), 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  ERROR %s: %v\n", rel, err)
		return failed
	}
	if !wrote {
		status(out, "OK", rel, "unchanged")
		return unchanged
	}
	status(out, "WRITE", rel, "")
	return written
}
