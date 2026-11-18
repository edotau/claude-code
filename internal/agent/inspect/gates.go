package inspect

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/source"
)

// Gates for `agents diff --check`: gofmt, build, vet, lint, complexity, test, docs; --gates adds yaml, shell, quality.

const (
	diffHeadLines   = 30
	diffGateTimeout = 10 * time.Minute
	outsideShown    = 3
	fileChunk       = 200
)

// Seams: production runs real processes; tests swap them so no go or lint process is spawned.
var (
	gateRun      = runGateCmd
	gateLookPath = exec.LookPath
	gateSimplify = source.RunSimplify
	gateBlobs    = gitBlobs
)

// gateInput carries what the gates share: the repo root, the Go packages and the files to check.
type gateInput struct {
	root, base string
	pkgs       []string          // Go packages to vet, lint and test; ./... when whole
	files      []string          // files the per-kind steps check: the change set, or the whole tree with --all
	srcs       []string          // changed source files: complexity and quality judge what the change introduced
	old        map[string]string // checked path -> its path at base; absent for a file new since base
	strict     bool              // --gates was given: a missing tool fails instead of skipping
	whole      bool              // --all without --path: run the quality gate over the repo's own changed files
}

// gateFn returns the gate's output and a non-empty skip reason when it did not apply; an error is a FAIL.
type gateFn func(ctx context.Context, in gateInput) (out, skip string, err error)

type gateDef struct {
	name       string
	needsBuild bool
	fn         gateFn
	parse      parseFn
}

func newGateInput(root, base string, files []DiffFile) gateInput {
	in := gateInput{root: root, base: base, old: map[string]string{}}
	pkgSet := map[string]bool{}
	for _, f := range files {
		addGateFile(&in, pkgSet, f.Path, f.Status == "D")
		addGateFile(&in, pkgSet, f.From, true)
		if f.Status != "A" && f.Status != "?" {
			in.old[f.Path] = cmp.Or(f.From, f.Path)
		}
	}
	in.pkgs = slices.Sorted(maps.Keys(pkgSet))
	slices.Sort(in.files)
	slices.Sort(in.srcs)
	return in
}

// addGateFile records p's package and, unless the file is gone, its place in the gofmt and complexity lists.
func addGateFile(in *gateInput, pkgSet map[string]bool, p string, gone bool) {
	if p == "" || slices.Contains(strings.Split(p, "/"), "testdata") {
		return
	}
	isGo := strings.HasSuffix(p, ".go")
	if dir := path.Dir(p); isGo && dirHasGo(filepath.Join(in.root, filepath.FromSlash(dir))) {
		pkgSet["./"+dir] = true
		if dir == "." {
			pkgSet["."] = true
			delete(pkgSet, "./.")
		}
	}
	if gone {
		return
	}
	in.files = append(in.files, p)
	if source.DetectLanguage(p) != "" {
		in.srcs = append(in.srcs, p)
	}
}

func dirHasGo(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(ents, func(e os.DirEntry) bool { return strings.HasSuffix(e.Name(), ".go") })
}

func runGates(ctx context.Context, root, base string, files []DiffFile) []GateResult {
	return runDefaultGates(ctx, newGateInput(root, base, files))
}

// runDefaultGates is the gate set without --gates: the Go gates on the touched packages, docs when Markdown changed.
func runDefaultGates(ctx context.Context, in gateInput) []GateResult {
	defs := gateDefs()
	if len(in.pkgs) == 0 {
		out := []GateResult{{Name: "go gates", Status: "skip", Reason: "no Go changes; run make lint for docs"}}
		if len(in.srcs) > 0 {
			out = append(out, runGate(ctx, defs[complexityIdx], in))
		}
		return appendDocs(ctx, out, in)
	}
	res := make([]GateResult, len(defs))
	for i, d := range defs {
		if d.needsBuild && res[buildIdx].Status == "FAIL" {
			res[i] = GateResult{Name: d.name, Status: "skip", Reason: "build failed"}
			continue
		}
		res[i] = runGate(ctx, d, in)
	}
	return appendDocs(ctx, res, in)
}

// appendDocs adds the docs gate after the others, only when the change set holds Markdown.
func appendDocs(ctx context.Context, res []GateResult, in gateInput) []GateResult {
	if len(match(in.files, stepFor("docs").globs)) == 0 {
		return res
	}
	return append(res, runGate(ctx, docsDef, in))
}

const (
	buildIdx      = 1
	complexityIdx = 4
)

// gateDefs lists the default gates in report order, run one after another: concurrent ones were not faster.
func gateDefs() []gateDef {
	return []gateDef{
		stepDef("gofmt", false),
		{"build", false, func(ctx context.Context, in gateInput) (string, string, error) {
			out, err := gateRun(ctx, in.root, "go", "build", "./...")
			return out, "", err
		}, parseGoOutput("build")},
		stepDef("vet", true),
		stepDef("golangci-lint", true),
		{"complexity", false, complexityGate, parseComplexity},
		{"test", true, testGate, parseTest},
	}
}

var docsDef = gateDef{"docs", false, docsGate, parseDocs}

// allGateDefs is every gate --gates can pick, in report order.
func allGateDefs() []gateDef {
	return append(gateDefs(), docsDef, stepDef("yaml", false), stepDef("shell", false),
		gateDef{"quality", false, qualityGate, parseQuality})
}

// GateNames lists the values --gates takes: the groups, then every gate.
func GateNames() []string {
	names := slices.Sorted(maps.Keys(gateGroups))
	for _, d := range allGateDefs() {
		names = append(names, d.name)
	}
	return names
}

// gateGroups are the names --gates expands: lint is the set `make lint` runs.
var gateGroups = map[string][]string{
	"lint": {"gofmt", "vet", "golangci-lint", "docs", "yaml", "shell", "quality"},
}

// parseGates expands --gates values (group or gate names, comma lists allowed) into a set; unknown names are an error.
func parseGates(vals []string) (map[string]bool, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	known := map[string]bool{}
	for _, d := range allGateDefs() {
		known[d.name] = true
	}
	want := map[string]bool{}
	for _, v := range vals {
		for n := range strings.SplitSeq(v, ",") {
			names := gateGroups[strings.TrimSpace(n)]
			if names == nil {
				names = []string{strings.TrimSpace(n)}
			}
			for _, g := range names {
				if !known[g] {
					return nil, fmt.Errorf("--gates %q: unknown gate (want lint or one of %s)", g, strings.Join(slices.Sorted(maps.Keys(known)), ", "))
				}
				want[g] = true
			}
		}
	}
	return want, nil
}

// runSelectedGates runs the picked gates in report order; build is a gate like any other here.
func runSelectedGates(ctx context.Context, in gateInput, want map[string]bool) []GateResult {
	var res []GateResult
	buildFailed := false
	for _, d := range allGateDefs() {
		if !want[d.name] {
			continue
		}
		if d.needsBuild && buildFailed {
			res = append(res, GateResult{Name: d.name, Status: "skip", Reason: "build failed", Findings: []Finding{}})
			continue
		}
		r := runGate(ctx, d, in)
		buildFailed = buildFailed || (d.name == "build" && r.Status == "FAIL")
		res = append(res, r)
	}
	return res
}

func runGate(ctx context.Context, d gateDef, in gateInput) GateResult {
	start := time.Now()
	out, skip, err := d.fn(ctx, in)
	r := GateResult{Name: d.name, Status: "ok", Findings: []Finding{},
		Seconds: time.Since(start).Round(10 * time.Millisecond).Seconds()}
	switch {
	case skip != "":
		r.Status, r.Reason, r.Seconds = "skip", skip, 0
	case err != nil:
		r.Status = "FAIL"
		r.Findings = capRows(d.name, d.parse(scrubPaths(out+errText(out, err), in.root)), diffHeadLines)
	}
	return r
}

func errText(out string, err error) string {
	if strings.TrimSpace(out) != "" {
		return ""
	}
	return err.Error()
}

// scrubPaths drops the repo root and shortens the temp dir and home to <tmp> and ~, so two runs print the same bytes.
func scrubPaths(s, root string) string {
	s = replaceDir(s, root, "")
	home, _ := os.UserHomeDir()
	s = replaceDir(s, os.TempDir(), "<tmp>/")
	return replaceDir(s, home, "~/")
}

// replaceDir swaps dir and its symlink-resolved form, each followed by a slash, for repl; "" and "/" are left alone.
func replaceDir(s, dir, repl string) string {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		return s
	}
	s = strings.ReplaceAll(s, dir+"/", repl)
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		s = strings.ReplaceAll(s, real+"/", repl)
	}
	return s
}

// stepDef is the gate that runs the table step of that name.
func stepDef(gate string, needsBuild bool) gateDef {
	st := stepFor(gate)
	return gateDef{gate, needsBuild, stepGate(st), st.parse}
}

// stepGate runs a table step: placeholders filled from the input, the tool by list args, a missing tool skips or fails.
func stepGate(st step) gateFn {
	return func(ctx context.Context, in gateInput) (string, string, error) {
		files, cmd := match(in.files, st.globs), st.cmd
		if in.whole && st.wholeCmd != nil {
			cmd = st.wholeCmd
		}
		switch {
		case slices.Contains(cmd, "{files}") && len(files) == 0:
			return "", "no " + st.kind + " files", nil
		case slices.Contains(cmd, "{pkgs}") && len(in.pkgs) == 0:
			return "", "no Go packages", nil
		}
		bin := st.cmd[0]
		if st.install != "" {
			var ok bool
			if bin, ok = findTool(in.root, bin); !ok {
				reason := fmt.Sprintf("%s not installed (%s)", st.cmd[0], st.install)
				if in.strict && !st.optional {
					return "", "", errors.New(reason)
				}
				return "", reason, nil
			}
		}
		return runStep(ctx, in, st, cmd, bin, files)
	}
}

// findTool prefers the repo's bin/<tool> (make installs mdsmith there) over the PATH.
func findTool(root, tool string) (string, bool) {
	if local := filepath.Join(root, "bin", tool); fileFound(local) {
		return local, true
	}
	_, err := gateLookPath(tool)
	return tool, err == nil
}

func fileFound(p string) bool {
	_, err := gateLookPath(p)
	return err == nil
}

// runStep runs the step's command, in chunks of fileChunk files when it takes {files}; stops at the first failure.
func runStep(ctx context.Context, in gateInput, st step, cmd []string, bin string, files []string) (string, string, error) {
	var all []string
	chunks := [][]string{nil}
	if slices.Contains(cmd, "{files}") {
		chunks = slices.Collect(slices.Chunk(files, fileChunk))
	}
	for _, chunk := range chunks {
		out, err := gateRun(ctx, in.root, bin, expandArgs(cmd[1:], chunk, in.pkgs)...)
		all = append(all, out)
		if err == nil && st.listsFiles && strings.TrimSpace(out) != "" {
			err = errors.New("files need formatting")
		}
		if err != nil {
			return strings.Join(all, ""), "", err
		}
	}
	return strings.Join(all, ""), "", nil
}

// expandArgs fills the two placeholders in place; an argument is never split or joined into a shell string.
func expandArgs(cmd, files, pkgs []string) []string {
	out := make([]string, 0, len(cmd)+len(files)+len(pkgs))
	for _, a := range cmd {
		switch a {
		case "{files}":
			out = append(out, files...)
		case "{pkgs}":
			out = append(out, pkgs...)
		default:
			out = append(out, a)
		}
	}
	return out
}

// testGate tests the touched packages, plus internal/schemas (it pins file writers) when production Go changed.
func testGate(ctx context.Context, in gateInput) (string, string, error) {
	const schemas = "./internal/schemas"
	prod := slices.ContainsFunc(match(in.files, []string{goGlob}), func(p string) bool { return !pack.IsTestPath(p) })
	if prod && !slices.Contains(in.pkgs, schemas) && !slices.Contains(in.pkgs, "./...") && dirHasGo(filepath.Join(in.root, "internal", "schemas")) {
		in.pkgs = append(slices.Clone(in.pkgs), schemas)
	}
	if len(in.pkgs) == 0 {
		return "", "no Go packages", nil
	}
	out, err := gateRun(ctx, in.root, "go", append([]string{"test", "-count=1"}, in.pkgs...)...)
	return out, "", err
}

// docsGate runs mdsmith (the `make lint-docs` check) and the memory formatter check on the changed Markdown.
func docsGate(ctx context.Context, in gateInput) (string, string, error) {
	if len(match(in.files, stepFor("docs").globs)) == 0 {
		return "", "no markdown files", nil
	}
	out, skip, err := stepGate(stepFor("docs"))(ctx, in)
	memOut, memErr := memoryFmtGate(in)
	if memErr != nil { // runGate lets skip win over an error, which would hide the memory failure
		skip = ""
	}
	return out + memOut, skip, errors.Join(err, memErr)
}

// memoryFmtGate runs `memory fmt --check`; it fails on the changed bank files it lists, or all of them with --all.
func memoryFmtGate(in gateInput) (string, error) {
	store := paths.CanonDeep(memory.StoreDir())
	changed := map[string]bool{}
	for _, p := range match(in.files, []string{mdGlob}) {
		abs := paths.CanonDeep(filepath.Join(in.root, filepath.FromSlash(p)))
		if rel, rerr := filepath.Rel(store, abs); rerr == nil && !strings.HasPrefix(rel, "..") {
			changed[abs] = true
		}
	}
	live := in.whole && paths.CanonDeep(in.root) == paths.CanonDeep(paths.ConfigDir()) // the whole bank, as make lint-docs
	if len(changed) == 0 && !live {
		return "", nil
	}
	var report bytes.Buffer
	if _, err := memory.FormatReport(&report, true); err != nil {
		return "", fmt.Errorf("memory fmt --check: %w", err)
	}
	var dirty []string
	for line := range strings.Lines(report.String()) {
		if line = strings.TrimSpace(line); changed[paths.CanonDeep(line)] || (live && filepath.IsAbs(line) && strings.HasSuffix(line, ".md")) {
			dirty = append(dirty, line+": memory fmt --check: would change (run `claude-code memory fmt`)\n")
		}
	}
	if len(dirty) == 0 {
		return "", nil
	}
	return strings.Join(dirty, ""), errors.New("memory fmt")
}

// complexityGate runs the `review simplify` engine in-process; only MEDIUM+ findings the change introduced fail it.
func complexityGate(ctx context.Context, in gateInput) (string, string, error) {
	if len(in.srcs) == 0 {
		return "", "no source files", nil
	}
	rep, stderr, err := runSimplify(in.root, in.srcs)
	if err != nil {
		return stderr, "", err
	}
	fresh, note := rep.findings(in.root), ""
	if len(fresh) > 0 {
		fresh, note = introduced(ctx, in, fresh)
	}
	lines := make([]string, 0, len(fresh)+len(rep.Failed)+1)
	if note != "" && len(fresh) > 0 {
		lines = append(lines, note)
	}
	for _, f := range fresh {
		lines = append(lines, f.line)
	}
	for _, f := range rep.Failed {
		lines = append(lines, f+" analysis failed")
	}
	if len(lines) == 0 {
		return "", "", nil
	}
	return strings.Join(lines, "\n"), "", errors.New("findings")
}

// qualityGate is `review simplify --fail` in-process: new comment and size violations; --all runs it as make does.
func qualityGate(_ context.Context, in gateInput) (string, string, error) {
	args := []string{"--fail"}
	if in.base != "" && in.base != "HEAD" {
		args = append(args, "--base", in.base)
	}
	if !in.whole {
		goFiles := match(in.srcs, []string{goGlob})
		if len(goFiles) == 0 {
			return "", "no Go changes", nil
		}
		for _, p := range goFiles {
			args = append(args, relToCwd(in.root, p))
		}
	}
	var out, errBuf bytes.Buffer
	if gateSimplify(args, &out, &errBuf) == 0 {
		return "", "", nil
	}
	return out.String() + errBuf.String(), "", errors.New("new violations")
}

// relToCwd names a repo file relative to the working directory, which is how the simplify engine reads git.
func relToCwd(root, p string) string {
	abs := filepath.Join(root, filepath.FromSlash(p))
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, abs); err == nil {
			return rel
		}
	}
	return abs
}

func runSimplify(dir string, rels []string) (rep simplifyReport, stderr string, err error) {
	args := []string{"--json", "--threshold", "medium"}
	for _, s := range rels {
		args = append(args, filepath.Join(dir, filepath.FromSlash(s)))
	}
	var out, errBuf bytes.Buffer
	code := gateSimplify(args, &out, &errBuf)
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		return rep, errBuf.String(), fmt.Errorf("review simplify exit %d: %v", code, err)
	}
	return rep, "", nil
}

// simplifyReport is the part of `review simplify --json` the gate reads.
type simplifyReport struct {
	Files []struct {
		File       string           `json:"file"`
		Complexity []source.Finding `json:"complexity"`
		Quality    struct {
			Smells []source.Smell `json:"smells"`
		} `json:"quality"`
	} `json:"files"`
	Failed []string `json:"analysis_failed"`
}

// runGateCmd runs one gate process in dir with its combined output; list args only, never a shell.
func runGateCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = proc.ExecWaitDelay
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}
