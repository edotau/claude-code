// `agents diff`: one deterministic answer to what changed, whether it stayed in the --path set, and the Go gates.

package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// binarySniffBytes is how much of a file's head is scanned for a NUL, as git does.
const binarySniffBytes = 8000

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
	for _, g := range rep.Gates {
		if g.Status == "FAIL" {
			return DiffExitGate
		}
	}
	if rep.Verify != nil && rep.Verify.Mismatch {
		return DiffExitGate
	}
	if len(rep.Outside) > 0 {
		return DiffExitOutside
	}
	return 0
}

// repoRoot walks up from dir to the directory holding .git (a file in a linked worktree); no subprocess.
func repoRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d, nil
		}
		if filepath.Dir(d) == d {
			return "", errors.New("not inside a git repository")
		}
	}
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
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
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

// countFileLines counts the lines of an untracked regular file; a NUL in the first block marks it binary, as git does.
func countFileLines(p string) (lines int, binary bool) {
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	buf := make([]byte, 64<<10)
	var last byte = '\n'
	for first := true; ; first = false {
		n, err := r.Read(buf)
		if first && bytes.IndexByte(buf[:min(n, binarySniffBytes)], 0) >= 0 {
			return 0, true
		}
		if n > 0 {
			lines += bytes.Count(buf[:n], []byte{'\n'})
			last = buf[n-1]
		}
		if err != nil {
			break
		}
	}
	if last != '\n' {
		lines++
	}
	return lines, false
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
