// `agents diff`: one deterministic answer to what changed, whether it stayed in the --path set, and the Go gates.

package inspect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/dev/review/source"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
)

// Exit codes of Diff; 1 is a failed gate or a --verify MISMATCH, 3 a changed file outside --path.
const (
	DiffExitGate    = 1
	DiffExitUsage   = 2
	DiffExitOutside = 3
)

type DiffOptions struct {
	Dir, Base string
	Paths     []string
	Verify    string
	Check     bool
	Batch     bool // --batch: merge this session's pending handbacks into one check
	SessionID string
	Gates     []string // --gates values; empty runs the default gate set
	All       bool     // --all: the per-kind steps cover the whole tree, not only the changed files
	JSON      bool
	Out, Err  io.Writer
}

// DiffFile is one changed file; Status is M A D R C T U or ? (untracked).
type DiffFile struct {
	Path    string `json:"path"`
	From    string `json:"from,omitempty"`
	Status  string `json:"status"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
	Test    bool   `json:"test"`
}

type DiffLines struct {
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
}

// DiffTotals sums the files, split into production and test code.
type DiffTotals struct {
	Files int `json:"files"`
	DiffLines
	Prod DiffLines `json:"prod"`
	Test DiffLines `json:"test"`
}

// GateResult is one gate's outcome; Status is ok, FAIL or skip.
type GateResult struct {
	Name     string    `json:"name"`
	Status   string    `json:"status"`
	Seconds  float64   `json:"seconds"`
	Reason   string    `json:"reason,omitempty"`
	Findings []Finding `json:"findings"`
}

// DiffReport is the whole answer, also the --format json document.
type DiffReport struct {
	Files           []DiffFile    `json:"files"`
	Totals          DiffTotals    `json:"totals"`
	Outside         []string      `json:"outside"`
	Gates           []GateResult  `json:"gates"`
	Verify          *VerifyReport `json:"verify,omitempty"`
	PreDirtyOutside []string      `json:"predirty_outside,omitempty"`

	dispatch string

	fromBrief bool     // --path came from the verified dispatch's brief Owned line: the text report is compact
	noScope   bool     // --verify with neither --path nor an Owned line: the text report is a short verdict
	all       bool     // --all: the file rows would list the dirty files, which the gates did not narrow to
	newDecls  []string // advisory rows for new tests and helpers; text report only, never a failed gate
}

// Diff prints the report for o; exit code: 0 clean, 1 gate failed or MISMATCH, 2 usage, 3 outside --path.
func Diff(o DiffOptions) int {
	ctx, cancel := context.WithTimeout(context.Background(), diffGateTimeout)
	defer cancel()
	want, err := diffGates(o)
	if err != nil {
		fmt.Fprintf(o.Err, "agents diff: %v\n", err)
		return DiffExitUsage
	}
	in, err := diffInputs(ctx, o)
	if err != nil {
		fmt.Fprintf(o.Err, "agents diff: %v\n", err)
		return DiffExitUsage
	}
	rep := DiffReport{Files: in.files, Totals: sumDiff(in.files), Outside: outsidePaths(in.files, in.sel),
		Gates: []GateResult{}, Verify: in.verify, fromBrief: in.fromBrief, noScope: in.noScope, all: o.All,
		PreDirtyOutside: in.preDirtyOutside, dispatch: in.dispatch}
	rep.Outside = mergeOutside(rep.Outside, rep.PreDirtyOutside)
	decls := startDeclRows(o, in)
	if err := checkDiffGates(ctx, o, in, want, &rep); err != nil {
		fmt.Fprintf(o.Err, "agents diff: %v\n", err)
		return DiffExitUsage
	}
	finishBatch(o, in, want, &rep)
	return printDiff(o, in, rep, decls)
}

func diffGates(o DiffOptions) (map[string]bool, error) {
	if !o.Check && (len(o.Gates) > 0 || o.All || o.Batch) {
		return nil, errors.New("--gates, --all and --batch need --check")
	}
	return parseGates(o.Gates)
}

func checkDiffGates(ctx context.Context, o DiffOptions, in diffIn, want map[string]bool, rep *DiffReport) error {
	if !o.Check || len(in.files) == 0 && !o.All && len(in.pending.Tasks) == 0 {
		return nil
	}
	gi, err := gatesInput(ctx, o, in, filesInPaths(in.files, rep.Outside), len(want) > 0)
	if err != nil {
		return err
	}
	if diffExit(*rep) == 0 {
		rep.Gates = checkedGates(ctx, gi, want, in.pending.Tasks...)
	} else {
		rep.Gates = append(runGatesFor(ctx, gi, want), pendingGates(ctx, gi.root, in.pending.Tasks)...)
	}
	return nil
}

func printDiff(o DiffOptions, in diffIn, rep DiffReport, decls <-chan []string) int {
	if o.JSON {
		if err := helptext.WriteJSON(o.Out, rep); err != nil {
			fmt.Fprintf(o.Err, "agents diff: %v\n", err)
			return DiffExitGate
		}
	} else {
		rep.newDecls = <-decls
		renderDiff(o, rep, len(in.sel) > 0)
	}
	return diffExit(rep)
}

// startDeclRows reads the new-declaration rows while the gates run; JSON output has none.
func startDeclRows(o DiffOptions, in diffIn) <-chan []string {
	if o.JSON {
		return nil
	}
	ch := make(chan []string, 1)
	go func() { ch <- newDeclRows(in) }()
	return ch
}

// newDeclRows lists the new tests and helpers of the changed Go files inside the scope, read from the worktree.
func newDeclRows(in diffIn) []string {
	var cs []source.GoChange
	for _, f := range in.files {
		if f.Status != "D" && (len(in.sel) == 0 || inPaths(in.sel, f.Path)) {
			cs = append(cs, source.GoChange{Path: f.Path, From: f.From})
		}
	}
	return source.GoAdvisory(in.root, proc.GitEnv(), in.base, false, cs).New
}

func runGatesFor(ctx context.Context, gi gateInput, want map[string]bool) []GateResult {
	if want == nil {
		return runDefaultGates(ctx, gi)
	}
	return runSelectedGates(ctx, gi, want)
}

// checkedGates only stamps a green run whose inputs stayed unchanged throughout validation.
func checkedGates(ctx context.Context, gi gateInput, want map[string]bool, tasks ...PendingTask) []GateResult {
	before, err := gateStampKeys(ctx, gi)
	res := append(runGatesFor(ctx, gi, want), pendingGates(ctx, gi.root, tasks)...)
	if len(gi.pkgs) == 0 || !greenGates(res) {
		return res
	}
	if err != nil {
		return append(res, GateResult{Name: "stamp", Status: "skip", Reason: err.Error()})
	}
	if err := saveGateStamps(ctx, gi, before, res); err != nil {
		res = append(res, GateResult{Name: "stamp", Status: "FAIL", Reason: err.Error()})
	}
	return res
}

type gateProof struct{ key, gates string }

func saveGateStamps(ctx context.Context, gi gateInput, before []gateProof, res []GateResult) error {
	after, err := gateStampKeys(ctx, gi)
	if err != nil {
		return err
	}
	if !slices.Equal(before, after) {
		return errors.New("files changed during validation; rerun the batch")
	}
	for _, proof := range after {
		if !provedGates(res, proof.gates) {
			continue
		}
		if err := WriteStamp(proof.key, StampMeta{Passed: true, Gates: proof.gates, Packages: gi.pkgs}); err != nil {
			return err
		}
	}
	return nil
}

func provedGates(res []GateResult, gates string) bool {
	for name := range strings.SplitSeq(gates, ",") {
		if !slices.ContainsFunc(res, func(g GateResult) bool { return g.Name == name && g.Status == "ok" }) {
			return false
		}
	}
	return true
}

func greenGates(res []GateResult) bool {
	return len(res) > 0 && !slices.ContainsFunc(res, func(g GateResult) bool { return g.Status != "ok" && g.Status != "skip" })
}

func gateStampKeys(ctx context.Context, gi gateInput) ([]gateProof, error) {
	if len(gi.pkgs) == 0 {
		return nil, nil
	}
	groups := [][]string{gi.pkgs}
	if len(gi.pkgs) > 1 {
		for _, p := range gi.pkgs {
			groups = append(groups, []string{p})
		}
	}
	var keys []gateProof
	for _, pkgs := range groups {
		key, err := ComputeStampKey(gi.root, pkgs, GoStampGates)
		if err != nil {
			return nil, err
		}
		keys = append(keys, gateProof{key, GoStampGates})
	}
	key, err := ComputeDiffStampKey(ctx, gi.root, gi.base, gi.pkgs, gi.files)
	return append(keys, gateProof{key, DefaultStampGates}), err
}

// gatesInput is the gates' input: the change set, or with --all every tracked and untracked file inside --path.
func gatesInput(ctx context.Context, o DiffOptions, in diffIn, changed []DiffFile, strict bool) (gateInput, error) {
	gi := newGateInput(in.root, in.base, changed)
	includeGoInputs(&gi, changed)
	gi.strict = strict
	if !o.All {
		return gi, nil
	}
	list, err := proc.GitNUL(ctx, in.root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return gi, err
	}
	var all []DiffFile
	for _, p := range list {
		if (len(in.sel) == 0 || inPaths(in.sel, p)) && paths.FileExists(filepath.Join(in.root, filepath.FromSlash(p))) {
			all = append(all, DiffFile{Path: p, Status: "M"})
		}
	}
	wide := newGateInput(in.root, in.base, all)
	includeGoInputs(&wide, all)
	wide.srcs, wide.old, wide.strict, wide.whole = gi.srcs, gi.old, strict, len(in.sel) == 0
	if wide.whole {
		wide.pkgs = []string{"./..."}
	}
	return wide, nil
}

// includeGoInputs resolves fixtures and package-local inputs before treating standalone Markdown as documentation.
func includeGoInputs(gi *gateInput, files []DiffFile) {
	if !paths.FileExists(filepath.Join(gi.root, "go.mod")) {
		return
	}
	for _, f := range files {
		if f.Path == "go.mod" || f.Path == "go.sum" {
			gi.pkgs = []string{"./..."}
			return
		}
		if pkg := goInputOwner(gi.root, f.Path); pkg != "" {
			gi.pkgs = append(gi.pkgs, pkg)
		}
	}
	slices.Sort(gi.pkgs)
	gi.pkgs = slices.Compact(gi.pkgs)
}

func goInputOwner(root, name string) string {
	dir := path.Dir(name)
	fixture := strings.HasPrefix(name, "testdata/")
	if fixture {
		dir = "."
	} else if fixtureRoot, _, found := strings.Cut(name, "/testdata/"); found {
		fixture = true
		dir = fixtureRoot
	}
	for {
		if dirHasGo(filepath.Join(root, dir)) && packageInput(root, dir, name, fixture) {
			if dir == "." {
				return "."
			}
			return "./" + dir
		}
		if dir == "." {
			return ""
		}
		dir = path.Dir(dir)
	}
}

func packageInput(root, dir, name string, fixture bool) bool {
	if fixture || path.Dir(name) == dir || !strings.HasSuffix(name, ".md") {
		return true
	}
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return true
	}
	rel := strings.TrimPrefix(name, dir+"/")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if embedsInput(filepath.Join(root, dir, e.Name()), rel) {
			return true
		}
	}
	return false
}

func embedsInput(file, rel string) bool {
	b, err := os.ReadFile(file)
	if err != nil {
		return true
	}
	patterns, err := stampEmbeds(string(b))
	match, matchErr := stampEmbedMatch(rel, patterns)
	return err != nil || matchErr != nil || match
}

// diffIn is what Diff resolves before it reports: the repo root, the cleaned --path list, the base commit and files.
type diffIn struct {
	root, base      string
	sel             []string
	files           []DiffFile
	verify          *VerifyReport
	fromBrief       bool
	noScope         bool
	preDirtyOutside []string
	dispatch        string
	pending         PendingVerify
	session         string
}

func diffInputs(ctx context.Context, o DiffOptions) (in diffIn, err error) {
	if in.root, err = repoRoot(o.Dir); err != nil {
		return
	}
	sel := slices.Clone(o.Paths)
	if o.Batch {
		if sel, err = batchPaths(o, &in, sel); err != nil {
			return
		}
	}
	if sel, err = diffVerifyScope(o.Verify, &in, sel); err != nil {
		return
	}
	if in.sel, err = cleanPaths(in.root, sel); err != nil {
		return
	}
	if in.base, err = resolveBase(ctx, in.root, o.Base); err != nil {
		return
	}
	in.files, err = collectChanges(ctx, in.root, in.base)
	if err == nil && needsLaneChanges(o, in) {
		in.base, in.files, err = laneChanges(ctx, in)
	}
	if err == nil {
		in.preDirtyOutside, in.dispatch = preDirtyOutside(in.root, in.sel, in.verify)
		addPendingFiles(&in)
	}
	return
}

func needsLaneChanges(o DiffOptions, in diffIn) bool {
	return !o.Batch && o.Base == "" && o.Check && len(filesInPaths(in.files, outsidePaths(in.files, in.sel))) == 0
}

func diffVerifyScope(id string, in *diffIn, sel []string) ([]string, error) {
	if id == "" {
		return sel, nil
	}
	var err error
	if in.verify, err = verifyDispatch(id, proxyLogPath()); err != nil {
		return nil, err
	}
	in.fromBrief = len(sel) == 0 && len(in.verify.owned) > 0
	if in.fromBrief {
		sel = ownedScope(in.verify.owned)
	}
	in.noScope = len(sel) == 0
	return sel, nil
}

func batchPaths(o DiffOptions, in *diffIn, sel []string) ([]string, error) {
	in.session = o.SessionID
	if in.session == "" {
		in.session = os.Getenv(paths.SessionIDEnv)
	}
	var err error
	if in.pending, err = ReadPending(in.session); err != nil {
		return nil, err
	}
	in.pending.Tasks = slices.DeleteFunc(in.pending.Tasks, func(task PendingTask) bool {
		return !paths.SamePath(in.root, task.Dir)
	})
	for _, task := range in.pending.Tasks {
		sel = append(sel, task.Files...)
	}
	if len(sel) == 0 && len(in.pending.Tasks) == 0 {
		return nil, errors.New("batch: no pending files; supply --path explicitly")
	}
	return sel, nil
}

func addPendingFiles(in *diffIn) {
	for _, task := range in.pending.Tasks {
		for _, p := range task.Files {
			if slices.ContainsFunc(in.files, func(f DiffFile) bool { return f.Path == p }) {
				continue
			}
			status := "M"
			if !paths.FileExists(filepath.Join(in.root, p)) {
				status = "D"
			}
			in.files = append(in.files, DiffFile{Path: p, Status: status, Test: pack.IsTestPath(p)})
		}
	}
	slices.SortFunc(in.files, func(a, b DiffFile) int { return strings.Compare(a.Path, b.Path) })
}

var pendingRun = runPendingCheck

func pendingGates(ctx context.Context, dir string, tasks []PendingTask) []GateResult {
	var out []GateResult
	seen := map[PendingCheck]bool{}
	for _, task := range tasks {
		for _, check := range task.Checks {
			if seen[check] {
				continue
			}
			seen[check] = true
			start := time.Now()
			text, err := pendingRun(ctx, dir, check)
			g := GateResult{Name: "verify " + task.Task + ": " + check.Command, Status: "ok", Seconds: time.Since(start).Seconds()}
			if err != nil {
				g.Status, g.Reason = "FAIL", text+"\n"+err.Error()
			}
			out = append(out, g)
		}
	}
	return out
}

func runPendingCheck(ctx context.Context, dir string, check PendingCheck) (string, error) {
	if check.Generated {
		return pendingCommand(ctx, dir, plans.VerifyStep{Argv: []string{"bash", "-c", "set -o pipefail; " + check.Command}})
	}
	if err := plans.VerifyShape(check.Command); err != nil {
		return "", err
	}
	steps, err := plans.VerifyArgv(check.Command)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	for _, step := range steps {
		text, err := pendingCommand(ctx, dir, step)
		output.WriteString(text)
		if err != nil {
			return output.String(), err
		}
	}
	return output.String(), nil
}

func pendingCommand(ctx context.Context, dir string, step plans.VerifyStep) (string, error) {
	cmd := exec.CommandContext(ctx, step.Argv[0], step.Argv[1:]...)
	cmd.Dir, cmd.Env = dir, proc.GoFlagsEnv(append(proc.GitEnv(), step.Env...), "-trimpath")
	proc.TreeCancel(cmd, time.Second)
	cmd.WaitDelay = proc.ExecWaitDelay
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func finishBatch(o DiffOptions, in diffIn, want map[string]bool, rep *DiffReport) {
	if !o.Batch || want != nil || diffExit(*rep) != 0 {
		return
	}
	if err := ClearPending(in.session, in.pending); err != nil {
		rep.Gates = append(rep.Gates, GateResult{Name: "pending queue", Status: "FAIL", Reason: err.Error()})
	}
}

// laneChanges re-reads changes against merge-base(main, HEAD) when none lie in --path, so a committed lane is checked.
func laneChanges(ctx context.Context, in diffIn) (string, []DiffFile, error) {
	mb, err := resolveBase(ctx, in.root, "main")
	if err != nil {
		return in.base, in.files, nil
	}
	files, err := collectChanges(ctx, in.root, mb)
	return mb, files, err
}

// ownedScope is a brief's Owned paths plus the _test.go of each owned Go source, which a package owns too.
func ownedScope(owned []string) []string {
	sel := slices.Clone(owned)
	for _, p := range owned {
		if src, ok := strings.CutSuffix(p, ".go"); ok && !pack.IsTestPath(p) {
			sel = append(sel, src+"_test.go")
		}
	}
	return sel
}

func diffExit(rep DiffReport) int {
	failed := slices.ContainsFunc(rep.Gates, func(g GateResult) bool { return g.Status == "FAIL" })
	switch {
	case failed || (rep.Verify != nil && rep.Verify.Mismatch):
		return DiffExitGate
	case len(rep.Outside) > 0:
		return DiffExitOutside
	}
	return 0
}

// repoRoot is the directory holding .git above dir (a file in a linked worktree); no subprocess.
func repoRoot(dir string) (string, error) {
	if top, _ := paths.CheckoutOf(dir); top != "" {
		return top, nil
	}
	return "", errors.New("not inside a git repository")
}

// cleanPaths turns each --path into a clean repo-relative slash path or glob; `..`, outside-root and comma values fail.
func cleanPaths(root string, sel []string) ([]string, error) {
	out := make([]string, 0, len(sel))
	for _, p := range sel {
		if strings.Contains(p, ",") {
			return nil, fmt.Errorf("--path %q: one path per --path; repeat --path instead of a comma list", p)
		}
		if filepath.IsAbs(p) {
			rel, err := filepath.Rel(root, p)
			if err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				rel, err = filepath.Rel(paths.CanonDeep(root), paths.CanonDeep(p)) // /tmp/x vs /private/tmp/x
			}
			if err != nil {
				return nil, fmt.Errorf("--path %q: %v", p, err)
			}
			p = rel
		}
		c := path.Clean(filepath.ToSlash(p))
		if p == "" || c == ".." || strings.HasPrefix(c, "../") || strings.HasPrefix(c, "/") {
			return nil, fmt.Errorf("--path %q: must stay inside the repo root", p)
		}
		out = append(out, c)
	}
	return out, nil
}

func inPaths(sel []string, p string) bool {
	for _, o := range sel {
		if o == "." || p == o || strings.HasPrefix(p, o+"/") || (strings.ContainsAny(o, "*?") && paths.GlobMatch(o, p)) {
			return true
		}
	}
	return false
}

// outsidePaths lists changed paths no --path covers; a rename counts its source too. Empty sel means no check.
func outsidePaths(files []DiffFile, sel []string) []string {
	out := []string{}
	if len(sel) == 0 {
		return out
	}
	for _, f := range files {
		if !inPaths(sel, f.Path) || (f.From != "" && !inPaths(sel, f.From)) {
			out = append(out, f.Path)
		}
	}
	return out
}

// filesInPaths drops the outside files, so another agent's edits never fail this agent's gates.
func filesInPaths(files []DiffFile, outside []string) []DiffFile {
	if len(outside) == 0 {
		return files
	}
	return slices.DeleteFunc(slices.Clone(files), func(f DiffFile) bool { return slices.Contains(outside, f.Path) })
}

func sumDiff(files []DiffFile) DiffTotals {
	t := DiffTotals{Files: len(files)}
	for _, f := range files {
		part := &t.Prod
		if f.Test {
			part = &t.Test
		}
		part.Added += f.Added
		part.Deleted += f.Deleted
		t.Added += f.Added
		t.Deleted += f.Deleted
	}
	return t
}

// resolveBase maps --base to the commit to diff against: HEAD, or the merge-base of ref and HEAD.
func resolveBase(ctx context.Context, root, base string) (string, error) {
	if base == "" || base == "HEAD" {
		return "HEAD", nil
	}
	if strings.HasPrefix(base, "-") {
		return "", fmt.Errorf("--base %q: must not start with '-'", base)
	}
	mb, err := proc.RunGit(ctx, root, "merge-base", base, "HEAD")
	if err != nil {
		return "", fmt.Errorf("--base %q: %w", base, err)
	}
	return mb, nil
}

// collectChanges is two git calls (diff --raw --numstat, ls-files --others) against the resolved base commit.
func collectChanges(ctx context.Context, root, base string) ([]DiffFile, error) {
	var diffOut string
	var othersOut []string
	var diffErr, othersErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		diffOut, diffErr = proc.RunGitRaw(ctx, root, "diff", "--raw", "--numstat", "-z", "-M", base, "--")
	})
	wg.Go(func() {
		othersOut, othersErr = proc.GitNUL(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	})
	wg.Wait()
	if err := errors.Join(diffErr, othersErr); err != nil {
		return nil, err
	}
	files, err := parseDiffZ(diffOut)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.Path] = true
	}
	for _, p := range othersOut {
		if !seen[p] {
			added, bin := countFileLines(filepath.Join(root, filepath.FromSlash(p)))
			files = append(files, DiffFile{Path: p, Status: "?", Added: added, Binary: bin, Test: pack.IsTestPath(p)})
		}
	}
	slices.SortFunc(files, func(a, b DiffFile) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}

// parseDiffZ reads `git diff --raw --numstat -z -M`: raw records (status, paths) first, then numstat records.
func parseDiffZ(out string) ([]DiffFile, error) {
	tok := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if len(tok) == 1 && tok[0] == "" {
		return []DiffFile{}, nil
	}
	files, next, err := parseRawRecords(tok)
	if err != nil {
		return nil, err
	}
	return files, applyNumstat(files, tok[next:])
}

// parseRawRecords reads the leading `:meta status path [path]` records and returns the index where numstat starts.
func parseRawRecords(tok []string) ([]DiffFile, int, error) {
	files := []DiffFile{}
	i := 0
	for ; i < len(tok) && strings.HasPrefix(tok[i], ":"); i++ {
		meta := strings.Fields(tok[i])
		if len(meta) == 0 || i+1 >= len(tok) {
			return nil, 0, fmt.Errorf("unexpected git diff output %q", tok[i])
		}
		f := DiffFile{Status: meta[len(meta)-1][:1]}
		if f.Status == "R" || f.Status == "C" {
			if i+2 >= len(tok) {
				return nil, 0, fmt.Errorf("truncated rename record %q", tok[i])
			}
			f.From = tok[i+1]
			i++
		}
		i++
		f.Path, f.Test = tok[i], pack.IsTestPath(tok[i])
		files = append(files, f)
	}
	return files, i, nil
}

// applyNumstat folds `added<TAB>deleted<TAB>path` records (a rename has an empty path, then old and new) into files.
func applyNumstat(files []DiffFile, tok []string) error {
	byPath := map[string]int{}
	for n, f := range files {
		byPath[f.Path] = n
	}
	for i := 0; i < len(tok); i++ {
		cols := strings.SplitN(tok[i], "\t", 3)
		if len(cols) < 3 {
			return fmt.Errorf("unexpected git numstat output %q", tok[i])
		}
		p := cols[2]
		if p == "" && i+2 < len(tok) {
			p = tok[i+2]
			i += 2
		}
		if n, ok := byPath[p]; ok {
			files[n].setCounts(cols[0], cols[1])
		}
	}
	return nil
}

func (f *DiffFile) setCounts(added, deleted string) {
	if added == "-" {
		f.Binary = true
		return
	}
	f.Added, _ = strconv.Atoi(added)
	f.Deleted, _ = strconv.Atoi(deleted)
}

func preDirtyOutside(root string, sel []string, v *VerifyReport) ([]string, string) {
	if len(sel) == 0 {
		return nil, ""
	}
	var touched *[]string
	var id string
	if v != nil {
		touched, id = v.PreDirty, v.ID
	} else if r, err := agent.LoadRecord(os.Getenv(proc.DispatchIDEnv)); err == nil {
		touched, id = r.PreDirtyTouched, r.ID
	}
	if touched == nil {
		return nil, id
	}
	var out []string
	for _, p := range *touched {
		if !filepath.IsAbs(p) {
			continue
		}
		rel, err := cleanPaths(root, []string{p})
		if err == nil && !inPaths(sel, rel[0]) {
			out = append(out, rel[0])
		}
	}
	slices.Sort(out)
	return slices.Compact(out), id
}

func mergeOutside(outside, touched []string) []string {
	if len(touched) == 0 {
		return outside
	}
	out := append(slices.Clone(outside), touched...)
	slices.Sort(out)
	return slices.Compact(out)
}
