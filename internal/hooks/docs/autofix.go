package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/autofix"
	"github.com/edotau/claude-code/internal/review/bench"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/worktree"
)

// autofixTimeout bounds one commit's repair, lane setup and leaf launch inside the detached verifier.
const autofixTimeout = 15 * time.Minute

// autofixLockWait is how long a run waits for another autofix to finish before it reports busy.
var autofixLockWait = 2 * time.Second

// Seams: a test swaps every one; none of the real ones may run under go test.
var (
	autofixRepair   = autofix.Repair
	autofixLeaf     = autofix.FixLeaf
	autofixLaneCap  = bench.LaneCap
	autofixBudget   = bench.GateFixBudget
	autofixReserve  = bench.Reserve
	autofixSpend    = bench.SpendDispatch
	autofixAttempts = autofix.Attempts
	autofixVerb     = autofixRunVerb
	autofixSettled  = dispatchSettled
)

// GateAutofix repairs a red post-commit gate on the primary trunk and returns its notice lines; a green gate is nil.
func GateAutofix(top, sha, gateOutput string, failureIDs ...string) []string {
	if strings.TrimSpace(gateOutput) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), autofixTimeout)
	defer cancel()
	short := sha[:min(7, len(sha))]
	if plans.ValidSHA(sha) {
		sha = primaryTrunkHead(ctx, top, sha)
	}
	if sha == "" || !plans.ValidSHA(sha) {
		return autofixNote(short, "skipped: not a commit on the primary checkout's trunk")
	}
	var lines []string
	// one run at a time machine-wide, so a second verifier for the same sha sees the first one's branch or ledger row
	err := paths.WithFileLockWithin(filepath.Join(paths.StateDir(), "autofix", "run"), autofixLockWait, func() error {
		lines = autofixLocked(ctx, top, sha, gateOutput, failureIDs)
		return nil
	})
	switch {
	case errors.Is(err, paths.ErrLockTimeout):
		return autofixNote(short, "autofix busy") // the other commit's gate is judged on its own run
	case err != nil:
		return autofixNote(short, "nothing done: run lock failed: "+firstErrLine(err))
	}
	return lines
}

// autofixNote builds a notice line; every notice passes autofixNotice, the one sanitiser.
func autofixNote(short, text string) []string {
	return []string{autofixNotice("autofix " + short + ": " + text)}
}

func autofixLocked(ctx context.Context, top, sha, gateOutput string, failureIDs []string) []string {
	short := sha[:7]
	var lines []string
	if why := autofixTried(ctx, top, sha); why != "" {
		return append(lines, autofixNote(short, why)...)
	}
	findings, dropped := autofix.LocalFindings(uniqueFindings(autofix.Classify(gateOutput)))
	if dropped > 0 {
		lines = append(lines, autofixNote(short, fmt.Sprintf("dropped %d finding(s) with a non-local path", dropped))...)
	}
	if len(findings) == 0 {
		return append(lines, autofixNote(short, "nothing done: no findings recognised in the gate output")...)
	}
	for _, id := range failureIDs {
		if got, err := worktree.ClaimFailure(worktree.FailuresDir(), id, "autofix-"+short); err != nil || got != "autofix-"+short {
			return append(lines, autofixNote(short, "nothing done: failure held by another session")...)
		}
	}
	var green autofix.Result
	if slices.ContainsFunc(findings, func(f autofix.Finding) bool { return f.Class.Deterministic() }) {
		var more []string
		green, more = autofixRunRepair(ctx, top, sha, findings)
		lines = append(lines, more...)
	}
	if slices.ContainsFunc(findings, func(f autofix.Finding) bool { return !f.Class.Deterministic() }) {
		req := autofix.LeafRequest{Sha: sha, Live: top, Gate: autofixGate(findings), Findings: findings}
		lines = append(lines, autofixJudgment(ctx, req, green)...)
	}
	return lines
}

// autofixRunRepair runs Repair and returns its green lane (zero when none) and its notices.
func autofixRunRepair(ctx context.Context, top, sha string, findings []autofix.Finding) (autofix.Result, []string) {
	res, err := autofixRepair(ctx, top, sha, findings)
	var lines []string
	if err != nil {
		lines = autofixNote(sha[:7], "repair failed: "+firstErrLine(err))
	}
	if res.Notice != "" {
		lines = append(lines, autofixNotice(res.Notice))
	}
	if err != nil || res.Lane == "" {
		return autofix.Result{}, lines
	}
	return res, lines
}

// autofixSweepTimeout bounds one whole sweep: a running leaf's settle wait inside SpendDispatch cannot stretch it.
var autofixSweepTimeout = 5 * time.Second

// SweepAutofixSpend records the tokens of today's and yesterday's fix leaves; another sweep running skips this one.
func SweepAutofixSpend(now time.Time) []string {
	var lines []string
	err := paths.WithFileLockWithin(filepath.Join(paths.StateDir(), "autofix", "sweep"), 0, func() error {
		lines = autofixSweepBounded(now)
		return nil
	})
	if err != nil && !errors.Is(err, paths.ErrLockTimeout) {
		return []string{autofixNotice("autofix: spend sweep skipped: lock failed")}
	}
	return lines
}

// autofixSweepBounded returns at the deadline even while one SpendDispatch still waits; the loop then stops.
func autofixSweepBounded(now time.Time) []string {
	ctx, cancel := context.WithTimeout(context.Background(), autofixSweepTimeout)
	defer cancel()
	done := make(chan []string, 1)
	go func() { done <- autofixSweep(ctx, now) }()
	select {
	case lines := <-done:
		return lines
	case <-ctx.Done():
		return []string{autofixNotice("autofix: spend sweep cut short at its time limit")}
	}
}

func autofixSweep(ctx context.Context, now time.Time) []string {
	rows, err := autofixAttempts()
	if err != nil {
		return []string{autofixNotice("autofix: spend sweep skipped: attempts ledger unreadable")}
	}
	done, err := autofixSwept()
	if err != nil {
		return []string{autofixNotice("autofix: spend sweep skipped: settled list unreadable")}
	}
	var failed int
	for _, row := range sweepRows(rows, done, now) {
		if ctx.Err() != nil {
			break
		}
		if autofixSpendOne(row) != nil {
			failed++
		}
	}
	if failed > 0 {
		return []string{autofixNotice(fmt.Sprintf("autofix: spend of %d fix leaf(s) not recorded", failed))}
	}
	return nil
}

func autofixSweptFile() string { return filepath.Join(paths.StateDir(), "autofix", "swept") }

// autofixSwept is the set of dispatch ids whose final spend is recorded; a missing file is empty.
func autofixSwept() (map[string]bool, error) {
	b, err := os.ReadFile(autofixSweptFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	for id := range strings.FieldsSeq(string(b)) {
		done[id] = true
	}
	return done, nil
}

// autofixSpendOne records one leaf's spend; settled before the spend means final tokens, so it is marked done.
func autofixSpendOne(row autofix.Attempt) error {
	settled := autofixSettled(row.DispatchID)
	// charged to the launch time, so a later sweep never moves a leaf's tokens into another day
	if err := autofixSpend(bench.KindGateFix, row.DispatchID, row.At); err != nil || !settled {
		return err
	}
	return autofixMarkSwept(row.DispatchID)
}

func autofixMarkSwept(id string) error { return paths.AppendLine(autofixSweptFile(), []byte(id)) }

// sweepRows is the first attempt row of each dispatch launched today or yesterday and not in done.
func sweepRows(rows []autofix.Attempt, done map[string]bool, now time.Time) []autofix.Attempt {
	days := []string{now.Format(time.DateOnly), now.AddDate(0, 0, -1).Format(time.DateOnly)}
	seen := map[string]bool{}
	var out []autofix.Attempt
	for _, row := range rows {
		day := row.At.In(now.Location()).Format(time.DateOnly)
		if row.DispatchID == "" || seen[row.DispatchID] || done[row.DispatchID] || !slices.Contains(days, day) {
			continue
		}
		seen[row.DispatchID] = true
		out = append(out, row)
	}
	return out
}

// dispatchSettled is whether the dispatch record has left "running"; SpendDispatch does not say, so read it first.
func dispatchSettled(id string) bool {
	if id != filepath.Base(id) || strings.Contains(id, "..") {
		return false
	}
	b, err := os.ReadFile(filepath.Join(paths.DispatchDir(), id+".json"))
	var rec struct {
		Status string `json:"status"`
	}
	if err != nil || json.Unmarshal(b, &rec) != nil {
		return false
	}
	return rec.Status != "" && rec.Status != "running"
}

// autofixTried is why sha already had its one attempt: a lane branch Repair left, or a fix-leaf ledger row.
func autofixTried(ctx context.Context, top, sha string) string {
	branch := "worktree-autofix-" + sha[:7]
	out, err := proc.RunGit(ctx, top, "branch", "--list", branch)
	if err != nil {
		return "nothing done: branch lookup failed"
	}
	if strings.TrimSpace(out) != "" {
		return "nothing done: already attempted (branch " + branch + ")"
	}
	rows, err := autofixAttempts()
	if err != nil {
		return "nothing done: attempts ledger unreadable"
	}
	if slices.ContainsFunc(rows, func(a autofix.Attempt) bool { return a.Sha == sha }) {
		return "nothing done: already attempted (fix-leaf ledger)"
	}
	return ""
}

// autofixJudgment launches the one fix leaf in Repair's green lane, else a fresh one it removes if nothing launched.
func autofixJudgment(ctx context.Context, req autofix.LeafRequest, green autofix.Result) []string {
	top, sha := req.Live, req.Sha
	short := sha[:7]
	if ok, why := autofixBudget(); !ok {
		return autofixNote(short, "fix leaf not started: budget refused: "+why)
	}
	lane, dir := green.Lane, green.Path
	if lane == "" {
		var err error
		if lane, dir, err = autofixNewLane(ctx, top, sha); err != nil {
			return autofixNote(short, "fix leaf not started: "+firstErrLine(err))
		}
	}
	req.Lane = dir
	att, err := autofixLeaf(ctx, req, func() (bool, string) {
		if ok, why := autofixBudget(); !ok {
			return false, why
		}
		return autofixReserve(bench.KindGateFix, sha, time.Now()) // counts toward the 6 a day before the launch
	})
	lines := autofixNote(short, autofixLeafText(lane, att, err))
	if att.Verdict != "dispatched" && green.Lane == "" {
		if _, cerr := autofixVerb(ctx, top, "worktree", "clean", lane); cerr != nil {
			lines = append(lines, autofixNote(short, "lane "+lane+" left: clean failed")...)
		}
	}
	return lines
}

func autofixNewLane(ctx context.Context, top, sha string) (lane, dir string, err error) {
	if err := autofixLaneCap(ctx, top); err != nil {
		return "", "", err
	}
	lane = "autofix-" + sha[:7]
	out, err := autofixVerb(ctx, top, "worktree", "create", lane)
	if err != nil {
		return "", "", errors.New("lane create failed")
	}
	rows := strings.Split(strings.TrimSpace(out), "\n")
	return lane, rows[len(rows)-1], nil
}

func autofixLeafText(lane string, att autofix.Attempt, err error) string {
	switch {
	case errors.Is(err, autofix.ErrAttempted):
		return "nothing done: already attempted (fix-leaf ledger)"
	case errors.Is(err, autofix.ErrBudget):
		return "fix leaf not started: " + firstErrLine(err)
	case att.Verdict == "dispatched":
		return "fix leaf " + att.DispatchID + " started in lane " + lane + "; it hands back uncommitted: claude-code agents result -f " +
			att.DispatchID
	case err != nil:
		return "fix leaf not started: " + firstErrLine(err)
	}
	return "nothing done: no judgment findings"
}

// autofixGate is the check the leaf reruns: the diff gate on the files its findings name.
func autofixGate(findings []autofix.Finding) string {
	files := []string{"claude-code agents diff --check"}
	for _, f := range findings {
		if !f.Class.Deterministic() && f.Path != "" {
			files = append(files, "--path "+f.Path)
		}
	}
	slices.Sort(files[1:])
	return strings.Join(slices.Compact(files), " ")
}

// uniqueFindings drops repeats: each task's saved diff output names the same rows.
func uniqueFindings(in []autofix.Finding) []autofix.Finding {
	seen := map[autofix.Finding]bool{}
	return slices.DeleteFunc(in, func(f autofix.Finding) bool {
		dup := seen[f]
		seen[f] = true
		return dup
	})
}

func firstErrLine(err error) string { return textutil.FirstLine(err.Error()) }

const maxNoticeRunes = 300

var (
	// reRunCmd is repairRun's "run <binary> <args>: " wrapper; the command line never reaches a notice.
	reRunCmd  = regexp.MustCompile(`\brun [^:\n]*: `)
	reAbsPath = regexp.MustCompile(`(^|[^\w.~/\\-])((?:[A-Za-z]:\\|/)[^\s"'(),;\[\]]*)`)
	reLane    = regexp.MustCompile(`(?:^|[/\\])(autofix-[0-9a-f]{7}(?:[/\\].*)?)$`)
)

// autofixNotice is the one sanitiser: first line, no command line, lanes by name, files repo-relative, capped.
func autofixNotice(line string) string {
	line = reRunCmd.ReplaceAllString(textutil.FirstLine(line), "")
	line = reAbsPath.ReplaceAllStringFunc(line, func(s string) string {
		m := reAbsPath.FindStringSubmatch(s)
		if lane := reLane.FindStringSubmatch(m[2]); lane != nil {
			return m[1] + strings.ReplaceAll(lane[1], `\`, "/")
		}
		return m[1] + path.Base(strings.ReplaceAll(m[2], `\`, "/"))
	})
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, line)
	return textutil.CapRunes(line, maxNoticeRunes, "…")
}

// autofixRunVerb runs a claude-code lane verb, refused from a test binary that would re-run itself.
func autofixRunVerb(ctx context.Context, top string, args ...string) (string, error) {
	bin, err := paths.SelfBinary()
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(bin, ".test") {
		return "", errors.New("lane verbs need the claude-code binary, not a test binary")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = top
	out, err := cmd.CombinedOutput()
	return string(out), err
}
