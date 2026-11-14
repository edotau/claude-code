// `agents diff`: one deterministic answer to what changed, whether it stayed in the --path set, and the Go gates.

package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Exit codes of Diff; 1 is a failed gate or a --verify MISMATCH, 3 a changed file outside --path.
const (
	DiffExitGate    = 1
	DiffExitUsage   = 2
	DiffExitOutside = 3
)

// DiffOptions configures Diff; Dir is any directory inside the repo.
type DiffOptions struct {
	Dir, Base string
	Paths     []string
	Verify    string
	Check     bool
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

// DiffLines is an added/deleted pair.
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
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Seconds    float64 `json:"seconds"`
	Reason     string  `json:"reason,omitempty"`
	OutputHead string  `json:"output_head"`
	CutLines   int     `json:"cut_lines"`
}

// DiffReport is the whole answer, also the --format json document.
type DiffReport struct {
	Files   []DiffFile    `json:"files"`
	Totals  DiffTotals    `json:"totals"`
	Outside []string      `json:"outside"`
	Gates   []GateResult  `json:"gates"`
	Verify  *VerifyReport `json:"verify,omitempty"`
}

// Diff prints the report for o and returns the exit code: 0 clean, 1 gate failed or MISMATCH, 2 usage, 3 outside --path.
func Diff(o DiffOptions) int {
	ctx, cancel := context.WithTimeout(context.Background(), diffGateTimeout)
	defer cancel()
	in, err := diffInputs(ctx, o)
	if err != nil {
		fmt.Fprintf(o.Err, "agents diff: %v\n", err)
		return DiffExitUsage
	}
	rep := DiffReport{Files: in.files, Totals: sumDiff(in.files), Outside: outsidePaths(in.files, in.sel),
		Gates: []GateResult{}, Verify: in.verify}
	if o.Check && len(in.files) > 0 {
		rep.Gates = runGates(ctx, in.root, in.base, filesInPaths(in.files, rep.Outside))
	}
	if o.JSON {
		if err := helptext.WriteJSON(o.Out, rep); err != nil {
			fmt.Fprintf(o.Err, "agents diff: %v\n", err)
			return DiffExitGate
		}
	} else {
		renderDiff(o, rep, len(in.sel) > 0)
	}
	return diffExit(rep)
}

// diffIn is what Diff resolves before it reports: the repo root, the cleaned --path list, the base commit and files.
type diffIn struct {
	root, base string
	sel        []string
	files      []DiffFile
	verify     *VerifyReport
}

func diffInputs(ctx context.Context, o DiffOptions) (in diffIn, err error) {
	if in.root, err = repoRoot(o.Dir); err != nil {
		return
	}
	if in.sel, err = cleanPaths(in.root, o.Paths); err != nil {
		return
	}
	if o.Verify != "" {
		if in.verify, err = verifyDispatch(o.Verify, proxyLogPath()); err != nil {
			return
		}
	}
	if in.base, err = resolveBase(ctx, in.root, o.Base); err != nil {
		return
	}
	in.files, err = collectChanges(ctx, in.root, in.base)
	return
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
		if o == "." || p == o || strings.HasPrefix(p, o+"/") || (strings.ContainsAny(o, "*?") && globMatches(o, p)) {
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
	var diffOut, othersOut string
	var diffErr, othersErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		diffOut, diffErr = proc.RunGitRaw(ctx, root, "diff", "--raw", "--numstat", "-z", "-M", base, "--")
	})
	wg.Go(func() {
		othersOut, othersErr = proc.RunGitRaw(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
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
	for p := range strings.SplitSeq(othersOut, "\x00") {
		if p != "" && !seen[p] {
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

// Go gates for `agents diff --check`: gofmt, build, vet, lint, complexity, test and docs on the touched packages.

const (
	diffHeadLines   = 30
	diffGateTimeout = 10 * time.Minute
	docsGateChunk   = 200
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
	root, base          string
	pkgs, goFiles, srcs []string
	mds                 []string          // changed Markdown files still present
	old                 map[string]string // checked path -> its path at base; absent for a file new since base
}

// gateFn returns the gate's output and a non-empty skip reason when it did not apply; an error is a FAIL.
type gateFn func(ctx context.Context, in gateInput) (out, skip string, err error)

type gateDef struct {
	name       string
	needsBuild bool
	fn         gateFn
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
	slices.Sort(in.goFiles)
	slices.Sort(in.srcs)
	slices.Sort(in.mds)
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
	if strings.HasSuffix(p, ".md") {
		in.mds = append(in.mds, p)
	}
	if isGo {
		in.goFiles = append(in.goFiles, p)
	}
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
	in := newGateInput(root, base, files)
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
	if len(in.mds) == 0 {
		return res
	}
	return append(res, runGate(ctx, gateDef{name: "docs", fn: docsGate}, in))
}

// Indexes into gateDefs.
const (
	buildIdx      = 1
	complexityIdx = 4
)

// gateDefs lists the gates in report order, run one after another: concurrent vet, lint and test were not faster.
func gateDefs() []gateDef {
	return []gateDef{
		{"gofmt", false, gofmtGate},
		{"build", false, func(ctx context.Context, in gateInput) (string, string, error) {
			out, err := gateRun(ctx, in.root, "go", "build", "./...")
			return out, "", err
		}},
		{"vet", true, pkgGate("go", "vet")},
		{"golangci-lint", true, lintGate},
		{"complexity", false, complexityGate},
		{"test", true, pkgGate("go", "test", "-count=1")},
	}
}

func runGate(ctx context.Context, d gateDef, in gateInput) GateResult {
	start := time.Now()
	out, skip, err := d.fn(ctx, in)
	r := GateResult{Name: d.name, Status: "ok", Seconds: time.Since(start).Round(10 * time.Millisecond).Seconds()}
	switch {
	case skip != "":
		r.Status, r.Reason, r.Seconds = "skip", skip, 0
	case err != nil:
		r.Status = "FAIL"
		r.OutputHead, r.CutLines = cutHead(scrubPaths(out+errText(out, err), in.root), diffHeadLines)
	}
	return r
}

// errText names the failure when the tool printed nothing (a missing binary, a timeout).
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

func cutHead(out string, limit int) (string, int) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) <= limit {
		return strings.Join(lines, "\n"), 0
	}
	return strings.Join(lines[:limit], "\n"), len(lines) - limit
}

func gofmtGate(ctx context.Context, in gateInput) (string, string, error) {
	if len(in.goFiles) == 0 {
		return "", "no Go files", nil
	}
	out, err := gateRun(ctx, in.root, "gofmt", append([]string{"-l"}, in.goFiles...)...)
	if err == nil && strings.TrimSpace(out) != "" {
		err = errors.New("files need gofmt")
	}
	return out, "", err
}

func pkgGate(name string, args ...string) gateFn {
	return func(ctx context.Context, in gateInput) (string, string, error) {
		if len(in.pkgs) == 0 {
			return "", "no Go packages", nil
		}
		out, err := gateRun(ctx, in.root, name, append(args, in.pkgs...)...)
		return out, "", err
	}
}

func lintGate(ctx context.Context, in gateInput) (string, string, error) {
	if _, err := gateLookPath("golangci-lint"); err != nil {
		return "", "not installed", nil
	}
	return pkgGate("golangci-lint", "run")(ctx, in)
}

// docsGate runs mdsmith (the `make lint-docs` check) and the memory formatter check on the changed Markdown.
func docsGate(ctx context.Context, in gateInput) (string, string, error) {
	out, skip, err := mdsmithGate(ctx, in)
	memOut, memRan, memErr := memoryFmtGate(in)
	if skip != "" && memRan {
		skip = ""
	}
	return out + memOut, skip, errors.Join(err, memErr)
}

// mdsmithGate runs the repo's mdsmith on the changed Markdown only.
func mdsmithGate(ctx context.Context, in gateInput) (string, string, error) {
	bin := filepath.Join(in.root, "bin", "mdsmith")
	if _, err := gateLookPath(bin); err != nil {
		bin = "mdsmith"
		if _, err := gateLookPath(bin); err != nil {
			return "", "mdsmith not installed", nil
		}
	}
	var all []string
	for chunk := range slices.Chunk(in.mds, docsGateChunk) {
		out, err := gateRun(ctx, in.root, bin, append([]string{"check", "--no-color", "--"}, chunk...)...)
		all = append(all, out)
		if err != nil {
			return strings.Join(all, ""), "", err
		}
	}
	return strings.Join(all, ""), "", nil
}

// memoryFmtGate runs the `memory fmt --check` package function and fails on the changed memory-store files it lists.
func memoryFmtGate(in gateInput) (out string, ran bool, err error) {
	store := paths.Canon(memory.StoreDir())
	changed := map[string]bool{}
	for _, p := range in.mds {
		abs := paths.Canon(filepath.Join(in.root, filepath.FromSlash(p)))
		if rel, rerr := filepath.Rel(store, abs); rerr == nil && !strings.HasPrefix(rel, "..") {
			changed[abs] = true
		}
	}
	if len(changed) == 0 {
		return "", false, nil
	}
	var report bytes.Buffer
	if _, err := memory.FormatReport(&report, true); err != nil {
		return "", true, fmt.Errorf("memory fmt --check: %w", err)
	}
	var dirty []string
	for line := range strings.Lines(report.String()) {
		if line = strings.TrimSpace(line); changed[paths.Canon(line)] {
			dirty = append(dirty, line)
		}
	}
	if len(dirty) == 0 {
		return "", true, nil
	}
	return strings.Join(dirty, "\n") + "\nmemory fmt --check: would change (run `claude-code memory fmt`)\n", true,
		errors.New("memory fmt")
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

// runSimplify runs the detector on rels under dir at the medium threshold.
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

// `agents diff --verify <id>`: routing proof for one dispatch, from its record and the proxy log.

const (
	verifyChunk     = 256 << 10
	verifyReadCap   = 8 << 20
	verifyTailBytes = 8 << 10
	windowSlack     = 2 * time.Second
)

// verifyScanCap bounds how many bytes of the proxy log a verify reads from its end; a var so a test can shrink it.
var verifyScanCap int64 = 16 << 20

// proxyLogPath is the loopback proxy's one-line-per-request log.
func proxyLogPath() string { return filepath.Join(paths.StateDir(), "proxy.log") }

// VerifyTokens is a dispatch's token usage as its job log reports it.
type VerifyTokens struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

// ProxyMatch counts the proxy-log requests attributed to one dispatch; Match says how exact that attribution is.
type ProxyMatch struct {
	Match    string         `json:"match"`
	Requests int            `json:"requests"`
	Remote   int            `json:"remote"`
	Hosts    []string       `json:"hosts"`
	Broken   int            `json:"stream_broken"`
	NonOK    map[string]int `json:"non_200"`
	Note     string         `json:"note"`
}

// VerifyReport is the routing proof for one dispatch (`agents diff --verify <id>`).
type VerifyReport struct {
	ID         string        `json:"id"`
	Agent      string        `json:"agent"`
	Workspace  string        `json:"workspace"`
	Route      string        `json:"route,omitempty"`
	Model      string        `json:"model"`
	Status     string        `json:"status"`
	Fallback   string        `json:"fallback"`
	Start      string        `json:"start"`
	End        string        `json:"end"`
	DurationMs int64         `json:"duration_ms"`
	Tokens     *VerifyTokens `json:"tokens,omitempty"`
	Proxy      ProxyMatch    `json:"proxy"`
	Mismatch   bool          `json:"mismatch"`
	Mismatches []string      `json:"mismatches,omitempty"`
}

// verifyDispatch builds the routing proof for dispatch id from its record and the proxy log at logPath.
func verifyDispatch(id, logPath string) (*VerifyReport, error) {
	rec, err := LoadRecord(id)
	if err != nil {
		return nil, fmt.Errorf("--verify %q: %w", id, err)
	}
	start := rec.Time.Truncate(time.Second)
	end := time.Now()
	if rec.DurationMs > 0 {
		end = rec.Time.Add(time.Duration(rec.DurationMs) * time.Millisecond)
	}
	rep := &VerifyReport{ID: rec.ID, Agent: rec.Agent, Workspace: rec.Provider, Route: rec.Route, Model: rec.Model,
		Status: rec.Status, Fallback: fallbackText(rec), DurationMs: rec.DurationMs, Tokens: logTokens(rec.LogPath()),
		Start: start.Local().Format(time.RFC3339), End: end.Local().Format(time.RFC3339),
		Mismatches: routingMismatches(rec)}
	rep.Mismatch = len(rep.Mismatches) > 0
	rep.Proxy = matchProxy(logPath, rec, start, end.Add(windowSlack))
	return rep, nil
}

func fallbackText(rec Record) string {
	if rec.Fallback == "" && rec.RanAs == "" {
		return "none"
	}
	s := rec.Fallback + " -> " + cmp.Or(rec.RanAs, rec.Agent)
	if rec.FallbackReason != "" {
		s += " (" + rec.FallbackReason + ")"
	}
	return s
}

// routingMismatches lists where the run differs from the request: a fallback, another workspace, another model.
func routingMismatches(rec Record) []string {
	var out []string
	if rec.Fallback != "" || rec.RanAs != "" {
		out = append(out, "fallback: "+fallbackText(rec))
	}
	if p := rec.Request.Provider; p != "" && !strings.Contains(rec.Provider+" "+rec.Route, p) {
		out = append(out, fmt.Sprintf("workspace: asked for %q, ran on %q", p, cmp.Or(rec.Route, rec.Provider)))
	}
	return append(out, modelMismatch(rec)...)
}

// modelMismatch compares the requested model with the one that ran; an alias that is part of the real name passes.
func modelMismatch(rec Record) []string {
	m := rec.Request.Model
	if m == "" || rec.Model == "" || strings.Contains(rec.Model+" "+rec.Invoked, m) {
		return nil
	}
	return []string{fmt.Sprintf("model: asked for %q, ran %q", m, rec.Model)}
}

var tokensRe = regexp.MustCompile(`tokens in=(\d+) out=(\d+)`)

// logTokens reads the last "tokens in=N out=M" line from the tail of the dispatch's job log.
func logTokens(logPath string) *VerifyTokens {
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(0, st.Size()-verifyTailBytes)
	buf, _ := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
	all := tokensRe.FindAllSubmatch(buf, -1)
	if len(all) == 0 {
		return nil
	}
	last := all[len(all)-1]
	in, _ := strconv.ParseInt(string(last[1]), 10, 64)
	out, _ := strconv.ParseInt(string(last[2]), 10, 64)
	return &VerifyTokens{In: in, Out: out}
}

type proxyLine struct {
	ts                                         time.Time
	profile, model, status, via, req, dispatch string
	broken                                     bool
}

// parseProxyLine reads a proxy log line: fixed fields, then via=, dispatch=, req=, stream=broken[:class] tokens.
func parseProxyLine(line string) (proxyLine, bool) {
	f := strings.Fields(line)
	if len(f) < 4 {
		return proxyLine{}, false
	}
	ts, err := time.Parse(time.RFC3339, f[0])
	if err != nil {
		return proxyLine{}, false
	}
	pl := proxyLine{ts: ts, profile: f[1], model: f[2], status: f[3]}
	for _, kv := range f[4:] {
		if strings.HasPrefix(kv, "msg=") { // the upstream error text is quoted and last: never read metadata from it
			break
		}
		pl.setToken(kv)
	}
	return pl, true
}

func (pl *proxyLine) setToken(kv string) {
	switch {
	case strings.HasPrefix(kv, "via="):
		pl.via = kv[4:]
	case strings.HasPrefix(kv, "dispatch="):
		pl.dispatch = kv[9:]
	case strings.HasPrefix(kv, "req="):
		pl.req = kv[4:]
	case kv == "stream=broken" || strings.HasPrefix(kv, "stream=broken:"):
		pl.broken = true
	}
}

// scanBackward calls visit on each line of r from the last to the first until visit returns false.
func scanBackward(r io.ReaderAt, size int64, visit func(line string) bool) error {
	var carry []byte
	for end := size; end > 0; {
		start := max(0, end-verifyChunk)
		buf := make([]byte, end-start, end-start+int64(len(carry)))
		if _, err := r.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		lines := bytes.Split(append(buf, carry...), []byte{'\n'})
		first := 0
		if start > 0 {
			carry, first = lines[0], 1
		}
		for i := len(lines) - 1; i >= first; i-- {
			if len(lines[i]) > 0 && !visit(string(lines[i])) {
				return nil
			}
		}
		end = start
	}
	return nil
}

// matchProxy counts the proxy lines of rec's profile and model inside [start, end]; request ids make it exact.
func matchProxy(logPath string, rec Record, start, end time.Time) ProxyMatch {
	pm := ProxyMatch{Match: "none", Hosts: []string{}, NonOK: map[string]int{}}
	if rec.Provider == "" || rec.Model == "" {
		pm.Note = "the record names no provider or model, so it has no proxy profile to match"
		return pm
	}
	tagged, hits, capped, err := windowLines(logPath, rec, start, end)
	if err != nil {
		pm.Note = "proxy log unreadable: " + err.Error()
		return pm
	}
	capNote := ""
	if capped {
		capNote = fmt.Sprintf("; scan stopped after %d bytes of the proxy log: older lines not read", verifyScanCap)
	}
	if len(tagged) > 0 {
		pm.Match, pm.Note = "dispatch id", "matched by dispatch= tags on the proxy log lines: exact"+capNote
		tally(&pm, tagged)
		return pm
	}
	pm.Match = "time window"
	pm.Note = fmt.Sprintf("no proxy log line carries dispatch=%s; fell back to the time window over untagged lines "+
		"(%s %s, %s..%s): counts may include other traffic on the same workspace and model", rec.ID, rec.Provider,
		rec.Model, start.Local().Format("15:04:05"), end.Local().Format("15:04:05"))
	if exact := exactLines(rec, hits); len(exact) > 0 {
		hits, pm.Match, pm.Note = exact, "request id", "matched by req= ids that also appear in the dispatch's own log: exact"
	}
	tally(&pm, hits)
	pm.Note += capNote
	return pm
}

// proxyKind is "tagged" for a dispatch=<rec.ID> line, "window" for an untagged in-range line of rec's workspace, else "".
func proxyKind(pl proxyLine, rec Record, start, end time.Time) string {
	switch {
	case pl.dispatch == rec.ID:
		return "tagged"
	case pl.dispatch == "" && !pl.ts.Before(start) && !pl.ts.After(end) && pl.profile == rec.Provider && pl.model == rec.Model:
		return "window"
	}
	return ""
}

// windowLines reads the proxy log from its end up to verifyScanCap bytes (capped says it was hit), never by timestamp.
func windowLines(logPath string, rec Record, start, end time.Time) (tagged, window []proxyLine, capped bool, err error) {
	f, err := os.Open(logPath)
	if err != nil {
		return nil, nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, false, err
	}
	var read int64
	err = scanBackward(f, st.Size(), func(line string) bool {
		if read += int64(len(line)) + 1; read > verifyScanCap {
			capped = true
			return false
		}
		if pl, ok := parseProxyLine(line); ok {
			switch proxyKind(pl, rec, start, end) {
			case "tagged":
				tagged = append(tagged, pl)
			case "window":
				window = append(window, pl)
			}
		}
		return true
	})
	return tagged, window, capped, err
}

// exactLines keeps the lines whose req= id appears in the dispatch's job log or event file.
func exactLines(rec Record, hits []proxyLine) []proxyLine {
	if !slices.ContainsFunc(hits, func(l proxyLine) bool { return l.req != "" }) {
		return nil
	}
	var text strings.Builder
	for _, p := range []string{rec.LogPath(), rec.EventsPath()} {
		if f, err := os.Open(p); err == nil {
			_, _ = io.Copy(&text, io.LimitReader(f, verifyReadCap))
			_ = f.Close()
		}
	}
	return slices.DeleteFunc(slices.Clone(hits), func(l proxyLine) bool {
		return l.req == "" || !strings.Contains(text.String(), l.req)
	})
}

func tally(pm *ProxyMatch, hits []proxyLine) {
	hosts := map[string]bool{}
	for _, h := range hits {
		pm.Requests++
		if h.via != "" {
			pm.Remote++
			hosts[h.via] = true
		}
		if h.broken {
			pm.Broken++
		}
		if h.status != "200" {
			pm.NonOK[h.status]++
		}
	}
	pm.Hosts = slices.Sorted(maps.Keys(hosts))
}

func renderDiff(o DiffOptions, rep DiffReport, scoped bool) {
	if len(rep.Files) == 0 {
		fmt.Fprintln(o.Out, "agents diff: no changes")
		renderVerify(o.Out, rep.Verify)
		return
	}
	for _, f := range rep.Files {
		counts, tag := fmt.Sprintf("+%d -%d", f.Added, f.Deleted), ""
		if f.Binary {
			counts = "bin"
		}
		if f.Test {
			tag = " (test)"
		}
		name := f.Path
		if f.From != "" {
			name = f.From + " -> " + f.Path
		}
		fmt.Fprintf(o.Out, "%s %-12s %s%s\n", f.Status, counts, name, tag)
	}
	if len(rep.Outside) > 0 {
		fmt.Fprintf(o.Out, "outside --path (%d):\n", len(rep.Outside))
		for _, p := range rep.Outside {
			fmt.Fprintf(o.Out, "  %s\n", p)
		}
	}
	renderGates(o, rep.Gates)
	renderVerify(o.Out, rep.Verify)
	fmt.Fprintln(o.Out, diffSummary(rep, scoped))
}

func renderGates(o DiffOptions, gates []GateResult) {
	codes, color := map[string]string{"ok": ansi.Green, "FAIL": ansi.Red, "skip": ansi.Dim}, ansi.Enabled(o.Out)
	for _, g := range gates {
		status := ansi.Style(color, codes[g.Status], g.Status)
		if g.Status == "skip" {
			fmt.Fprintf(o.Out, "%s %s (%s)\n", status, g.Name, g.Reason)
		} else {
			fmt.Fprintf(o.Out, "%s %s (%.1fs)\n", status, g.Name, g.Seconds)
		}
		if g.OutputHead == "" {
			continue
		}
		for l := range strings.SplitSeq(g.OutputHead, "\n") {
			fmt.Fprintf(o.Out, "    %s\n", l)
		}
		if g.CutLines > 0 {
			fmt.Fprintf(o.Out, "    ... %d more lines cut\n", g.CutLines)
		}
	}
}

func renderVerify(w io.Writer, v *VerifyReport) {
	if v == nil {
		return
	}
	ws := v.Workspace
	if v.Route != "" {
		ws += " (" + v.Route + ")"
	}
	fmt.Fprintf(w, "verify %s:\n  ran on: %s, workspace %s, model %s\n  fallback: %s\n", v.ID, v.Agent, ws, v.Model, v.Fallback)
	took := fmt.Sprintf("  took %s (%s..%s), status %s", (time.Duration(v.DurationMs) * time.Millisecond).Round(time.Second),
		v.Start, v.End, v.Status)
	if v.Tokens != nil {
		took += fmt.Sprintf(", tokens in=%d out=%d", v.Tokens.In, v.Tokens.Out)
	}
	fmt.Fprintln(w, took)
	p := v.Proxy
	fmt.Fprintf(w, "  proxy: %d requests; %s\n", p.Requests, p.Note)
	if p.Match != "none" {
		fmt.Fprintf(w, "    via remote host: %d (%s)\n    stream=broken: %d\n    non-200: %s\n", p.Remote,
			cmp.Or(strings.Join(p.Hosts, ", "), "none"), p.Broken, nonOKText(p.NonOK))
	}
	for _, m := range v.Mismatches {
		fmt.Fprintf(w, "  MISMATCH %s\n", m)
	}
}

func nonOKText(m map[string]int) string {
	if len(m) == 0 {
		return "0"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s x%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func diffSummary(rep DiffReport, scoped bool) string {
	t := rep.Totals
	s := fmt.Sprintf("agents diff: %d files +%d -%d (prod +%d -%d, test +%d -%d)", t.Files, t.Added, t.Deleted,
		t.Prod.Added, t.Prod.Deleted, t.Test.Added, t.Test.Deleted)
	switch {
	case len(rep.Outside) > 0:
		s += fmt.Sprintf("; %d outside --path", len(rep.Outside))
	case scoped:
		s += "; --path ok"
	}
	if len(rep.Gates) == 0 {
		return s
	}
	n := map[string]int{}
	for _, g := range rep.Gates {
		n[g.Status]++
	}
	s += fmt.Sprintf("; gates %d ok", n["ok"])
	if n["FAIL"] > 0 {
		s += fmt.Sprintf(", %d FAIL", n["FAIL"])
	}
	if n["skip"] > 0 {
		s += fmt.Sprintf(", %d skip", n["skip"])
	}
	return s
}
