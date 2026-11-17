// `agents diff`: one deterministic answer to what changed, whether it stayed in the --path set, and the Go gates.

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
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

	fromBrief bool // --path came from the verified dispatch's brief Owned line: the text report is compact
	noScope   bool // --verify with neither --path nor an Owned line: the text report is a short verdict
}

// Diff prints the report for o; exit code: 0 clean, 1 gate failed or MISMATCH, 2 usage, 3 outside --path.
func Diff(o DiffOptions) int {
	ctx, cancel := context.WithTimeout(context.Background(), diffGateTimeout)
	defer cancel()
	in, err := diffInputs(ctx, o)
	if err != nil {
		fmt.Fprintf(o.Err, "agents diff: %v\n", err)
		return DiffExitUsage
	}
	rep := DiffReport{Files: in.files, Totals: sumDiff(in.files), Outside: outsidePaths(in.files, in.sel),
		Gates: []GateResult{}, Verify: in.verify, fromBrief: in.fromBrief, noScope: in.noScope}
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
	fromBrief  bool
	noScope    bool
}

func diffInputs(ctx context.Context, o DiffOptions) (in diffIn, err error) {
	if in.root, err = repoRoot(o.Dir); err != nil {
		return
	}
	sel := o.Paths
	if o.Verify != "" {
		if in.verify, err = verifyDispatch(o.Verify, proxyLogPath()); err != nil {
			return
		}
		if in.fromBrief = len(sel) == 0 && len(in.verify.owned) > 0; in.fromBrief {
			sel = ownedScope(in.verify.owned)
		}
		in.noScope = len(sel) == 0
	}
	if in.sel, err = cleanPaths(in.root, sel); err != nil {
		return
	}
	if in.base, err = resolveBase(ctx, in.root, o.Base); err != nil {
		return
	}
	in.files, err = collectChanges(ctx, in.root, in.base)
	return
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
