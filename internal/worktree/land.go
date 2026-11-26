package worktree

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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/bench"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// ErrRebaseConflict marks a rebase that conflicted and was aborted, leaving the lane as it was.
var ErrRebaseConflict = errors.New("rebase conflict")

// LandResult reports one terminal transition ("integrate" or "promote"); ResumeHint is set when the caller must retry.
type LandResult struct {
	Phase      string
	BeforeOID  string
	AfterOID   string
	ResumeHint string
}

// integrateTarget rebases the lane onto the pinned target inside the lane (never repo.Main); a lane on it is kept.
func integrateTarget(ctx context.Context, plan *LandPlan) (*LandResult, error) {
	if issue, err := checkWorktreeState(ctx, plan.WorktreePath, plan.WorktreeGitDir, plan.AfterResolution); err != nil {
		return nil, err
	} else if issue != nil {
		return nil, fmt.Errorf("integrate: %s: %v", issue.Code, issue.Detail)
	}
	before, err := headOfContext(ctx, plan.WorktreePath)
	if err != nil {
		return nil, err
	}
	onTarget, err := isAncestor(ctx, plan.WorktreePath, plan.TargetOID, before)
	if err != nil {
		return nil, err
	}
	if onTarget {
		plan.IntegratedOID = before
		return &LandResult{Phase: "integrate", BeforeOID: before, AfterOID: before}, nil
	}
	out, err := runGit(ctx, plan.WorktreePath, nil, "-c", "rebase.autoStash=false", "rebase", "--no-autostash",
		"--no-update-refs", "--no-rebase-merges", "--no-fork-point", plan.TargetOID)
	if err != nil {
		return nil, abortRebase(ctx, plan, before, out, err)
	}
	after, err := headOfContext(ctx, plan.WorktreePath)
	if err != nil {
		return nil, err
	}
	plan.IntegratedOID = after
	return &LandResult{Phase: "integrate", BeforeOID: before, AfterOID: after}, nil
}

// abortRebase undoes a failed rebase so the lane is exactly as before; a conflict records and names its files.
func abortRebase(ctx context.Context, plan *LandPlan, before string, out []byte, cause error) error {
	failed := fmt.Errorf("git rebase inside lane: %w (%s)", cause, firstNonEmpty(gitErrorDetail(cause), string(out)))
	op, pending, err := inProgressOp(ctx, plan.WorktreePath, plan.WorktreeGitDir)
	if err != nil || !pending || op != "rebase" {
		return errors.Join(failed, err)
	}
	conflicts, listErr := runGit(ctx, plan.WorktreePath, nil, "diff", "--name-only", "--diff-filter=U", "-z")
	// a canceled land must still abort, so the abort gets its own timeout
	abortCtx, cancel := gitTimeoutCtx()
	defer cancel()
	if abortOut, err := runGit(abortCtx, plan.WorktreePath, nil, "rebase", "--abort"); err != nil {
		return errors.Join(failed, fmt.Errorf("git rebase --abort in %s failed, abort by hand: %w (%s)",
			plan.WorktreePath, err, firstNonEmpty(gitErrorDetail(err), string(abortOut))))
	}
	files := textutil.SplitNUL(string(conflicts))
	if listErr != nil || len(files) == 0 {
		return errors.Join(fmt.Errorf("%w; rebase aborted", failed), listErr)
	}
	plan.Issues = []Issue{{Code: IssueConflict, Paths: files}}
	return fmt.Errorf("%w in %s: %s; rebase aborted, lane left at %s; resolve it in the lane "+
		"(--resolve-agent <agent> or a fix leaf), then re-run worktree land", ErrRebaseConflict, plan.WorktreePath,
		strings.Join(files, ", "), ShortOID(before))
}

func promoteLocked(ctx context.Context, repo *Repo, plan *LandPlan) (*LandResult, error) {
	if plan.IntegratedOID == "" {
		return nil, errors.New("promote: no verified integration OID pinned — call integrateTarget first")
	}
	if err := promoteStateChecks(ctx, repo, plan); err != nil {
		return nil, err
	}
	freshTarget, freshSource, res, err := promoteFreshHeads(ctx, repo, plan)
	if err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		// never start the apply once canceled: repo.Main stays untouched
		return nil, err
	}
	return &LandResult{Phase: "land", BeforeOID: freshTarget, AfterOID: freshSource}, applyLaneDiff(ctx, repo.Main, freshTarget, freshSource)
}

// promoteStateChecks requires the pinned target, no overlapping target edits, and a clean source worktree.
func promoteStateChecks(ctx context.Context, repo *Repo, plan *LandPlan) error {
	branch, err := requireBranch(ctx, repo.Main)
	if err != nil {
		return fmt.Errorf("promote: %w", err)
	}
	if branch != plan.TargetRef {
		return fmt.Errorf("promote: main checkout moved to %q, pinned target was %q — retry from a fresh plan", branch, plan.TargetRef)
	}
	if issue, err := checkTargetState(ctx, repo.Main, plan.MainGitDir, plan.TargetOID+".."+plan.IntegratedOID); err != nil {
		return err
	} else if issue != nil {
		if issue.Code == issueDirtyTarget {
			return fmt.Errorf("promote: %s: %v: %w", issue.Code, issue.Detail, ErrLandRefused)
		}
		return fmt.Errorf("promote: %s: %v", issue.Code, issue.Detail)
	}
	if issue, err := checkWorktreeState(ctx, plan.WorktreePath, plan.WorktreeGitDir, plan.AfterResolution); err != nil {
		return err
	} else if issue != nil {
		return fmt.Errorf("promote: %s: %v", issue.Code, issue.Detail)
	}
	return laneLiveCheck(plan.WorktreePath)
}

// laneLiveCheck refuses while a dispatch job or a process has its cwd in the lane.
func laneLiveCheck(lane string) error {
	self := ancestry(os.Getpid())
	rec, ok, err := agent.LiveJobIn(lane, self)
	if err != nil {
		return fmt.Errorf("promote: read dispatch records: %w", err)
	}
	if ok {
		return errors.New("promote: leaf or job not finished: job " + rec.ID + " is live in the lane")
	}
	in := liveLister()
	if in == nil {
		return nil
	}
	p, err := foreignProc(in, paths.CanonDeep(lane), self)
	if err != nil {
		return err
	}
	if p != nil {
		return fmt.Errorf("promote: leaf or job not finished: pid %d (%s) is live in the lane", p.PID, p.Comm)
	}
	return nil
}

// promoteFreshHeads re-reads both heads and requires them to match the pinned integration and a fast-forward.
func promoteFreshHeads(ctx context.Context, repo *Repo, plan *LandPlan) (target, source string, res *LandResult, err error) {
	freshTarget, err := headOfContext(ctx, repo.Main)
	if err != nil {
		return "", "", nil, err
	}
	freshSource, err := headOfContext(ctx, plan.WorktreePath)
	if err != nil {
		return "", "", nil, err
	}
	if freshTarget != plan.TargetOID {
		return "", "", &LandResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "target moved since integration — re-run worktree land to re-integrate and retry"},
			fmt.Errorf("promote: %w from %s to %s since integration", errTargetMoved, plan.TargetOID, freshTarget)
	}
	if freshSource != plan.IntegratedOID {
		return "", "", &LandResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "worktree changed since the verified integration — re-run worktree land to re-integrate and retry"},
			fmt.Errorf("promote: worktree HEAD moved from the verified integration %s to %s since integration — refusing to promote an unreviewed commit",
				plan.IntegratedOID, freshSource)
	}

	ancestor, err := isAncestor(ctx, repo.Main, freshTarget, freshSource)
	if err != nil {
		return "", "", nil, fmt.Errorf("promote: %w", err)
	}
	if !ancestor {
		return "", "", &LandResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "not a fast-forward — re-run worktree land to re-integrate and retry"},
			fmt.Errorf("promote: %s is not an ancestor of the integrated worktree HEAD %s", freshTarget, freshSource)
	}
	return freshTarget, freshSource, nil, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// LandRequest is one `worktree land` apply; Out/Err get the progress lines, the Show* hooks render plans and results.
type LandRequest struct {
	Name, Target, ResolveAgent, Verify    string
	waited                                time.Duration
	Resume, NoClean, FullVerify, NoVerify bool
	Out, Err                              io.Writer
	ShowPlan                              func(*LandPlan)
	ShowResolved                          func(*LandResult)
	AfterLand                             func(laneHead string)
}

// landOutcome is a finished land: Blocked means the plan was refused (already shown); Clean is nil when NoClean.
type landOutcome struct {
	Blocked bool
	Clean   *CleanResult
}

// ShortOID is the 8-char form of an object id used in progress lines.
func ShortOID(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	return oid
}

// Land runs the lifecycle under the caller's lease: plan or resume, gate, rebase, verify, land, clean.
func Land(ctx context.Context, repo *Repo, req LandRequest) (out *landOutcome, err error) {
	err = withLandRun(ctx, repo, req.Name, req.Out, func(waited time.Duration) error {
		req.waited = waited
		out, err = land(ctx, repo, req)
		return err
	})
	return out, err
}

func land(ctx context.Context, repo *Repo, req LandRequest) (*landOutcome, error) {
	defer beginScanMemo()()
	var plan *LandPlan
	var err error
	if req.Resume {
		plan, err = resumeLand(ctx, repo, req)
	} else {
		plan, err = PlanOrResolve(ctx, repo, req, false)
	}
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return &landOutcome{Blocked: true}, nil
	}
	gate, gateSource, err := chooseVerifyGate(plan.WorktreePath, req.Verify, req.NoVerify)
	if err != nil {
		return nil, err
	}
	return applyLand(ctx, repo, plan, req, gate, gateSource)
}

// applyLand runs the lane rebase -> the gate (if any) -> promote with re-checks -> an optional clean, inside the lease.
func applyLand(ctx context.Context, repo *Repo, plan *LandPlan, req LandRequest, gate, gateSource string) (out *landOutcome, err error) {
	rec := startAttempt(repo, req)
	defer func() { rec.finish(err) }()
	plan, integrated, err := integrateLane(ctx, repo, plan, req)
	if err != nil {
		return nil, fmt.Errorf("integrate: %w", err)
	}
	rec.pin(plan, &rec.a.Seconds.Rebase)
	printRebase(req.Out, plan, integrated)

	var h *HeldGate
	if gate != "" && (usesGateSlot(gate, gateSource) || wantsScope(plan.WorktreePath, req, gateSource)) {
		h = HoldGate(ctx, repo.Main, plan.IntegratedOID, req.Name, req.Out)
		rec.stage(&rec.a.Seconds.Wait)
	}
	defer func() { h.Release() }()
	gate, gateSource, first, err := scopeGate(ctx, req, h.At(plan), gate, gateSource)
	if err != nil {
		return nil, err
	}
	if !usesGateSlot(gate, gateSource) {
		h.Release()
		h = nil
	}
	rec.a.Scope = scopeOf(gate, gateSource, req)
	rec.scoped(first)
	if err := runGate(ctx, req, h.At(plan), gate, gateSource, rec); err != nil {
		return nil, err
	}

	l := &relander{repo: repo, plan: plan, req: req, gate: gate, source: gateSource, first: first, rec: rec, h: h}
	landed, err := l.promoteLoop(ctx)
	if err != nil {
		return nil, err
	}
	return afterLand(ctx, repo, plan, req, landed)
}

// printRebase prints the rebase line of one integration.
func printRebase(out io.Writer, plan *LandPlan, r *LandResult) {
	if r.BeforeOID == r.AfterOID {
		fmt.Fprintf(out, "rebase:     skipped, lane already on %s\n", plan.TargetRef)
		return
	}
	fmt.Fprintf(out, "rebased:    %s -> %s in %s\n", ShortOID(r.BeforeOID), ShortOID(r.AfterOID), plan.WorktreePath)
}

// wantsScope reports whether scopeGate narrows a Makefile test gate: not with FullVerify or without a go.mod.
func wantsScope(dir string, req LandRequest, gateSource string) bool {
	return gateSource == "Makefile" && !req.FullVerify && paths.FileExists(filepath.Join(dir, "go.mod"))
}

// scopeGate narrows a Makefile test gate to the changed Go packages at at, returning what that gate covers.
func scopeGate(ctx context.Context, req LandRequest, at gateAt, gate, gateSource string) (g, source string, sc gateScope, err error) {
	if !wantsScope(at.Dir, req, gateSource) {
		return gate, gateSource, sc, nil
	}
	scoped, pkgs, nChanged, sc, err := scopedGoGate(ctx, at.Dir, at.Base)
	if err != nil {
		return "", "", sc, fmt.Errorf("scoped gate: %w", err)
	}
	if !sc.Scoped {
		// the whole suite is the Makefile gate itself, not a hard-coded go test
		return gate, gateSource, sc, nil
	}
	return scoped, fmt.Sprintf("%s, tests: %d changed + %d direct importers (full suite runs in the post-land audit)",
		scopedGateSource, nChanged, len(pkgs)-nChanged), sc, nil
}

// testTally passes writes through and tallies bare go test ok and FAIL lines, even split across writes.
type testTally struct {
	w      io.Writer
	passed int
	failed []string
	built  []string // packages whose "# pkg" header was followed by an error line
	header string   // the "# pkg" header whose lines are being read
	errs   bool     // an error line followed header
	line   []byte   // the current line, capped at tallyLineCap
}

// tallyHeader matches the "# <import path>" line go build and go vet print above a package's errors.
var tallyHeader = regexp.MustCompile(`^# (\S+)`)

// tallyResult matches bare "ok  \t<import path>" and "FAIL\t<import path>" lines (also "[build failed]").
var tallyResult = regexp.MustCompile(`^(ok *\t|FAIL[ \t]+)(\S+)`)

// tallyError matches a compiler or vet "file:line: message" line that is not a warning or note.
var tallyError = regexp.MustCompile(`^(vet: )?[^\s:]+:\d+(:\d+)?: (\S)`)
var tallyWarn = regexp.MustCompile(`^(vet: )?[^\s:]+:\d+(:\d+)?: (warning|note):`)

// tallyLineCap bounds the bytes kept of one line; a package line fits well within it.
const tallyLineCap = 512

func (c *testTally) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	for _, b := range p[:n] {
		if b == '\n' {
			c.flush()
		} else if len(c.line) < tallyLineCap {
			c.line = append(c.line, b)
		}
	}
	return n, err
}

// flush classifies the buffered line and starts the next one.
func (c *testTally) flush() {
	line := c.line
	c.line = c.line[:0]
	if h := tallyHeader.FindSubmatch(line); h != nil {
		c.endHeader()
		c.header = string(h[1])
	} else if c.header != "" && tallyError.Match(line) && !tallyWarn.Match(line) {
		c.errs = true
	}
	m := tallyResult.FindSubmatch(line)
	switch {
	case m == nil:
	case m[1][0] == 'o':
		c.passed++
	case !slices.Contains(c.failed, string(m[2])):
		c.failed = append(c.failed, string(m[2]))
	}
}

// endHeader keeps the open "# pkg" header as a failed build when an error line followed it.
func (c *testTally) endHeader() {
	if c.errs {
		c.built = appendNew(c.built, c.header)
	}
	c.header, c.errs = "", false
}

// runGate runs the verify gate at at (or announces its absence), recording seconds and package results in rec.
func runGate(ctx context.Context, req LandRequest, at gateAt, gate, gateSource string, rec *attempt) error {
	if gate == "" {
		fmt.Fprintln(req.Out, "verify: skipped (--no-verify) — promoting unverified")
		return nil
	}
	tally, errTally := &testTally{w: req.Out}, &testTally{w: req.Err}
	req.Out, req.Err = tally, errTally
	stages := gateStages(gate, gateSource)
	fmt.Fprintf(req.Out, "verify: %s\n", gate)
	if len(stages) > 1 {
		fmt.Fprintln(req.Out, "verify: build, then vet and tests")
	}
	secs, err := runStages(ctx, at, stages, req.Out, req.Err)
	rec.gated(secs)
	for _, t := range []*testTally{tally, errTally} {
		t.flush()
		t.endHeader()
	}
	rec.a.Tests += tally.passed
	if err != nil {
		rec.a.FailedPkgs = appendNew(rec.a.FailedPkgs, slices.Concat(tally.failed, tally.built, errTally.built, errTally.failed)...)
		rec.gateFail = gateFailText(gate, err)
		fmt.Fprintf(req.Err, "worktree left on disk at %s for inspection\n", at.Lane)
		return fmt.Errorf("verify failed, not promoting: %w", err)
	}
	fmt.Fprintf(req.Out, "verify: passed (%s)\n", gateSource)
	return nil
}

// appendNew appends each of add that dst does not hold yet.
func appendNew(dst []string, add ...string) []string {
	for _, p := range add {
		if !slices.Contains(dst, p) {
			dst = append(dst, p)
		}
	}
	return dst
}

// gateFailText is "verify `<cmd>` exited <code>", the command cut so the broadcast summary stays within its 160 bytes.
func gateFailText(gate string, err error) string {
	code := "with an error"
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		code = "exited " + strconv.Itoa(ee.ExitCode())
	}
	return "verify `" + oneLine(gate, 60) + "` " + code
}

// PlanOrResolve runs Preflight; a predicted conflict proceeds to the lane rebase; nil plan = blocked.
func PlanOrResolve(ctx context.Context, repo *Repo, req LandRequest, dryRun bool) (*LandPlan, error) {
	plan, err := Preflight(ctx, repo, req.Name, req.Target)
	if err != nil {
		return nil, err
	}
	req.ShowPlan(plan)
	if len(plan.Issues) > 0 && !onlyConflictIssue(plan.Issues) {
		return nil, dirtyRefusal(plan.Issues)
	}
	if dryRun {
		return plan, previewLand(ctx, repo, plan, req)
	}
	return plan, nil
}

// dirtyRefusal is the ErrLandRefused for a dirty-target issue, else nil (the plan is blocked and already shown).
func dirtyRefusal(issues []Issue) error {
	for _, iss := range issues {
		if iss.Code == issueDirtyTarget {
			return fmt.Errorf("%w: %s dirty in the live tree; nothing changed, lane kept", ErrLandRefused, strings.Join(iss.Paths, ", "))
		}
	}
	return nil
}

// integrateLane rebases the lane; a conflict notices its session, or with ResolveAgent is resolved in the lane.
func integrateLane(ctx context.Context, repo *Repo, plan *LandPlan, req LandRequest) (*LandPlan, *LandResult, error) {
	result, err := integrateTarget(ctx, plan)
	if !errors.Is(err, ErrRebaseConflict) {
		return plan, result, err
	}
	if req.ResolveAgent == "" {
		notifyConflict(plan, req.Err)
		return nil, nil, err
	}
	fmt.Fprintf(req.Out, "conflict:   %s; resolving in the lane with %s\n", issuePaths(plan.Issues), req.ResolveAgent)
	fresh, err := resolveConflict(ctx, repo, plan, req)
	if err != nil {
		return nil, nil, err
	}
	result, err = integrateTarget(ctx, fresh)
	return fresh, result, err
}

// resolveConflict hands the conflicted plan to the resolve agent, then re-plans and requires the target unmoved.
func resolveConflict(ctx context.Context, repo *Repo, plan *LandPlan, req LandRequest) (*LandPlan, error) {
	origTargetRef, origTargetOID := plan.TargetRef, plan.TargetOID
	_, result, err := resolveWithAgent(ctx, repo, plan, req.ResolveAgent, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	req.ShowResolved(result)
	return replanPinned(ctx, repo, req, origTargetRef, origTargetOID, "agent resolution", "during resolution")
}

// resumeLand reattaches to the saved --resolve-agent operation without re-invoking the agent and re-plans.
func resumeLand(ctx context.Context, repo *Repo, req LandRequest) (*LandPlan, error) {
	op, worktreePath, err := loadResumable(repo, req.Name)
	if err != nil {
		return nil, err
	}
	plan := &LandPlan{WorktreePath: worktreePath}
	_, result, err := resolveWithAgent(ctx, repo, plan, op.Agent, op)
	if err != nil {
		return nil, err
	}
	req.ShowResolved(result)
	return replanPinned(ctx, repo, req, op.TargetRef, op.TargetOID, "resuming resolution", "since the operation started")
}

// replanPinned re-plans a committed resolution, requiring it unblocked and the target still at the pinned ref and OID.
func replanPinned(ctx context.Context, repo *Repo, req LandRequest, ref, oid, blockedAfter, movedWhen string) (*LandPlan, error) {
	fresh, err := preflightAfterResolution(ctx, repo, req.Name, req.Target)
	if err != nil {
		return nil, err
	}
	if len(fresh.Issues) > 0 {
		req.ShowPlan(fresh)
		if err := dirtyRefusal(fresh.Issues); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("still blocked after %s (%s)", blockedAfter, issueCodes(fresh.Issues))
	}
	if fresh.TargetRef != ref || fresh.TargetOID != oid {
		return nil, fmt.Errorf("target moved from %s (%s) to %s (%s) %s — re-run worktree land explicitly to retry against the new target",
			ref, ShortOID(oid), fresh.TargetRef, ShortOID(fresh.TargetOID), movedWhen)
	}
	return fresh, nil
}

// notifyConflict appends a conflict notice to the session owning the lane's ledger; a failure only warns.
func notifyConflict(plan *LandPlan, errOut io.Writer) {
	s := ledgerSession(plan.WorktreePath)
	if s == "" {
		return
	}
	err := Append(inboxFor(s, plan.WorktreePath), Notice{TS: time.Now().Unix(), Kind: "conflict", From: "worktree land",
		Text: fmt.Sprintf("rebase of %s onto %s hit a conflict in %s and was aborted, lane unchanged; resolve it there "+
			"or run `claude-code worktree land --resolve-agent codex %s`", plan.WorktreePath, plan.TargetRef,
			issuePaths(plan.Issues), filepath.Base(plan.WorktreePath))})
	if err != nil && errOut != nil {
		fmt.Fprintf(errOut, "worktree land: warning: conflict notice: %v\n", err)
	}
}

// issuePaths joins every path the issues name.
func issuePaths(issues []Issue) string {
	var files []string
	for _, iss := range issues {
		files = append(files, iss.Paths...)
	}
	return strings.Join(files, ", ")
}

// onlyConflictIssue reports whether every blocking Issue is a predicted conflict, the one case --resolve-agent acts on.
func onlyConflictIssue(issues []Issue) bool {
	return len(issues) > 0 && !slices.ContainsFunc(issues, func(iss Issue) bool { return iss.Code != IssueConflict })
}

func issueCodes(issues []Issue) string {
	codes := make([]string, len(issues))
	for i, iss := range issues {
		codes[i] = iss.Code
	}
	return strings.Join(codes, ", ")
}

// landLockFile serializes the land apply across `worktree land` runs, never external writers.
const landLockFile = "worktree-land.lock"

// landLockWait is how long acquire waits (when wait<=0) before errLandBusy; a var so tests need not sit it out.
var landLockWait = 2 * time.Minute

// landLockSlice is how long acquire waits between progress lines.
var landLockSlice = 15 * time.Second

// errLandBusy is returned when the land lock is not granted within the wait.
var errLandBusy = errors.New("worktree land: another promote holds the land lock")

// acquire runs fn holding the land lock (wait<=0: landLockWait), calling note per slice waited; not reentrant.
func acquire(wait time.Duration, note func(waited time.Duration, pid, by string), fn func() error) error {
	dir := paths.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if wait <= 0 {
		wait = landLockWait
	}
	lock := filepath.Join(dir, landLockFile)
	start := time.Now()
	who := []byte(strconv.Itoa(os.Getpid()) + "\n" + cmp.Or(os.Getenv(LandWaitEnv), "manual land") + "\n")
	for {
		slice := min(landLockSlice, max(wait-time.Since(start), 0))
		ran, err := paths.TryFileLock(lock, slice, func() error {
			_ = paths.AtomicWrite(lock+".who", who, 0o600)
			return fn()
		})
		if err != nil || ran {
			return err
		}
		if time.Since(start) >= wait {
			return errLandBusy
		}
		if note != nil {
			pid, by := leaseHolder(lock + ".who")
			note(time.Since(start), pid, by)
		}
	}
}

// previewLand prints what the apply would do, in order, and the lane's divergence; it changes nothing.
func previewLand(ctx context.Context, repo *Repo, plan *LandPlan, req LandRequest) error {
	verify := "skip (--no-verify), promote marked unverified"
	if gate, source, err := chooseVerifyGate(plan.WorktreePath, req.Verify, req.NoVerify); err != nil {
		verify = "refuse: " + err.Error()
	} else if gate != "" {
		verify = gate + " (" + source + ", scoped to changed Go packages unless --full-verify)"
	}
	clean := "remove the lane, keep its branch"
	if req.NoClean {
		clean = "keep the lane (--no-clean)"
	}
	fmt.Fprintf(req.Out, "would: rebase %s onto %s inside the lane; a conflict aborts it, lane unchanged (exit 3)\n",
		plan.WorktreePath, plan.TargetRef)
	fmt.Fprintf(req.Out, "would: verify in the lane: %s\n", verify)
	fmt.Fprintf(req.Out, "would: apply the lane's diff uncommitted to the live tree; refuse (exit 3) if a landed path is dirty; %s\n", clean)
	changed, err := targetTouchedPaths(ctx, repo.Main, plan.TargetOID+"..."+plan.SourceOID)
	if err != nil {
		return err
	}
	fmt.Fprintf(req.Out, "changed files: %s\n", strings.Join(changed, ", "))
	return previewLane(req.Out, repo, plan.WorktreePath)
}

// previewLane prints the lane's ahead/behind and every lane (or main's dirty edits) it shares changed files with.
func previewLane(out io.Writer, repo *Repo, lane string) error {
	entries, err := List(repo)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !paths.SamePath(e.Path, lane) {
			continue
		}
		fmt.Fprintf(out, "lane: +%d -%d\n", e.Ahead, e.Behind)
		for _, other := range slices.Sorted(maps.Keys(e.Overlaps)) {
			fmt.Fprintf(out, "overlaps %s: %s\n", other, strings.Join(e.Overlaps[other], ", "))
		}
	}
	return nil
}

// ErrLandRefused marks a land stopped before it changed the live tree: a landed path is dirty or the apply failed.
var ErrLandRefused = errors.New("land refused")

const gitlinkMode = "160000"

// gitlinkPatch matches a patch header touching a submodule entry, which git apply cannot update without an index.
var gitlinkPatch = regexp.MustCompile(`(?m)^(?:index \S+ ` + gitlinkMode + `|(?:old|new|deleted file|new file) mode ` + gitlinkMode + `)`)

// applyLaneDiff applies the net diff from..to unstaged: --check first, then an apply a cancel cannot cut short.
func applyLaneDiff(ctx context.Context, main, from, to string) error {
	patch, err := runGit(ctx, main, nil, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none",
		"--src-prefix=a/", "--dst-prefix=b/", "--no-relative", "--no-color", "--submodule=short", from, to, "--")
	if err != nil {
		return fmt.Errorf("git diff %s..%s: %w", ShortOID(from), ShortOID(to), err)
	}
	if len(patch) == 0 {
		return nil
	}
	if gitlinkPatch.Match(patch) {
		return fmt.Errorf("the lane changes a submodule, which cannot land uncommitted: %w", ErrLandRefused)
	}
	if err := refuseHiddenEdits(ctx, main, from, to); err != nil {
		return err
	}
	if out, err := gitApply(ctx, main, patch, "--check"); err != nil {
		return fmt.Errorf("git apply --check in %s: %s; nothing changed: %w", main, out, ErrLandRefused)
	}
	if out, err := gitApply(context.WithoutCancel(ctx), main, patch); err != nil {
		return fmt.Errorf("git apply in %s failed after a clean --check, so the live tree may be partly changed "+
			"(the lane and its branch are intact; compare with `git status`): %s: %w", main, out, err)
	}
	return nil
}

// refuseHiddenEdits refuses a landed path marked assume-unchanged or skip-worktree: git status cannot show its edits.
func refuseHiddenEdits(ctx context.Context, main, from, to string) error {
	files, err := laneFiles(ctx, main, from, to)
	if err != nil || len(files) == 0 {
		return err
	}
	out, err := runGit(ctx, main, nil, append([]string{"--literal-pathspecs", "ls-files", "-v", "-z", "--"}, files...)...)
	if err != nil {
		return fmt.Errorf("git ls-files in %s: %w", main, err)
	}
	for rec := range strings.SplitSeq(string(out), "\x00") {
		if tag, path, ok := strings.Cut(rec, " "); ok && !strings.Contains("HMRCK", tag) {
			return fmt.Errorf("%s is marked assume-unchanged or skip-worktree in the live tree; nothing changed: %w", path, ErrLandRefused)
		}
	}
	return nil
}

// gitApply feeds patch to `git apply --binary` in main (extra flags first) and returns its trimmed output.
func gitApply(ctx context.Context, main string, patch []byte, extra ...string) (string, error) {
	cmd := gitCmd(ctx, append(append([]string{"-C", main, "apply", "--binary", "--whitespace=nowarn"}, extra...), "-")...)
	cmd.Stdin = bytes.NewReader(patch)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// laneFiles lists every path the diff a..b touches, both sides of a rename.
func laneFiles(ctx context.Context, dir, a, b string) ([]string, error) {
	out, err := runGit(ctx, dir, nil, "--literal-pathspecs", "diff", "--name-only", "-z", "--no-renames",
		"--ignore-submodules=none", a, b, "--")
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only %s..%s: %w", ShortOID(a), ShortOID(b), err)
	}
	return textutil.SplitNUL(string(out)), nil
}

// afterLand reports the land, then removes the lane's worktree (its branch stays until clean) and starts the audit.
func afterLand(ctx context.Context, repo *Repo, plan *LandPlan, req LandRequest, landed *LandResult) (*landOutcome, error) {
	if req.AfterLand != nil {
		req.AfterLand(landed.AfterOID)
	}
	if err := bench.ApplyPending(repo.Main, req.Name); err != nil {
		fmt.Fprintf(req.Err, "worktree land: warning: bench pending: %v\n", err)
	}
	if err := Dequeue(plan.WorktreePath); err != nil {
		fmt.Fprintf(req.Err, "worktree land: warning: land queue: %v\n", err)
	}
	// the lane's rebased head is exactly live HEAD plus the landed diff
	if line := startAudit(repo.Main, landed.AfterOID); line != "" {
		fmt.Fprintln(req.Out, line)
	}
	branch, _ := currentWorktreeBranch(plan.WorktreePath)
	out := &landOutcome{}
	closing := LedgerClose{Via: "land", LandSHA: landed.AfterOID, Head: landed.AfterOID}
	if !req.NoClean {
		result, err := removeLanded(repo, req.Name, plan.WorktreePath, req.Err, closing)
		out.Clean = &result
		if err != nil {
			return nil, fmt.Errorf("clean: %w", err)
		}
	}
	printLanded(ctx, repo, req, landed, "branch "+branch+" kept")
	return out, nil
}

// printLanded prints the one-line result, the landed diff stat and where plan ticks went.
func printLanded(ctx context.Context, repo *Repo, req LandRequest, landed *LandResult, state string) {
	files, _ := targetTouchedPaths(ctx, repo.Main, landed.BeforeOID+".."+landed.AfterOID)
	fmt.Fprintf(req.Out, "landed %s: %d files uncommitted in %s; %s\n", req.Name, len(files), repo.Main, state)
	if stat, err := runGit(ctx, repo.Main, nil, "diff", "--stat", landed.BeforeOID, landed.AfterOID, "--"); err == nil {
		fmt.Fprint(req.Out, string(stat))
	}
	fmt.Fprintln(req.Out, "plan ticks: none at landing; they tick when you commit with Task: trailers or run `plan task --done`")
}

// removeLanded removes a landed lane's worktree once it is idle; its branch always stays until `worktree clean`.
func removeLanded(repo *Repo, name, dest string, log io.Writer, closing LedgerClose) (CleanResult, error) {
	result, err := beginClean(name, dest, log)
	if err != nil {
		return CleanResult{}, err
	}
	if _, reason := cleanBusyReason(dest, "", nil); reason != "" {
		result.Reason = reason
		return result, nil
	}
	if result.Branch == "" {
		result.Reason = "detached lane: no branch would keep its commits, worktree kept"
		return result, nil
	}
	return removeMerged(repo, name, dest, Entry{Head: headOf(dest), keepBranch: true}, result, log, closing), nil
}

// keptBranch is the branch name's ledger recorded when the lane opened, else worktree-<name>.
func keptBranch(main, name string) string {
	data, err := os.ReadFile(ledgerPath(main, name, time.Now()))
	if err != nil {
		return "worktree-" + name
	}
	branch := ""
	for line := range strings.Lines(string(data)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "- Branch: "); ok && v != "kept" && v != "deleted" {
			branch = v
		}
	}
	return cmp.Or(branch, "worktree-"+name)
}

// cleanLandedBranch deletes name's kept branch once its landed diff is in live HEAD; there is no worktree left.
func cleanLandedBranch(repo *Repo, name string, dryRun bool) (CleanResult, error) {
	branch := keptBranch(repo.Main, name)
	if !branchExists(repo.Main, branch) {
		return CleanResult{}, fmt.Errorf("worktree %q not found, and no kept branch %s", name, branch)
	}
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	result := CleanResult{Path: branch, Branch: branch}
	out, err := runGit(ctx, repo.Main, nil, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return result, fmt.Errorf("resolve %s: %w", branch, err)
	}
	oid := strings.TrimSpace(string(out))
	if reason, err := landedBranchReason(ctx, repo.Main, branch, oid); err != nil || reason != "" {
		result.Reason = reason
		return result, err
	}
	result.Removed = true
	if dryRun {
		return result, nil
	}
	if out, err := runGit(ctx, repo.Main, nil, "update-ref", "-d", "refs/heads/"+branch, oid); err != nil {
		result.Removed, result.Reason = false, strings.TrimSpace(string(out))
		return result, nil
	}
	result.BranchDeleted = true
	return result, nil
}

// landedBranchReason is why branch (at oid) must stay ("" = its changed files match HEAD and it is not checked out).
func landedBranchReason(ctx context.Context, main, branch, oid string) (string, error) {
	if at, err := runGit(ctx, main, nil, "for-each-ref", "--format=%(worktreepath)", "refs/heads/"+branch); err == nil && len(bytes.TrimSpace(at)) > 0 {
		return "branch " + branch + " is checked out at " + strings.TrimSpace(string(at)), nil
	}
	base, err := runGit(ctx, main, nil, "merge-base", "HEAD", oid)
	if err != nil {
		return "no common history with HEAD: " + err.Error(), nil
	}
	files, err := laneFiles(ctx, main, strings.TrimSpace(string(base)), oid)
	if err != nil || len(files) == 0 {
		return "", err
	}
	args := append([]string{"--literal-pathspecs", "diff", "--quiet", "--no-ext-diff", "--no-textconv",
		"--ignore-submodules=none", "HEAD", oid, "--"}, files...)
	if _, err := runGit(ctx, main, nil, args...); err != nil {
		if code, isExit := asExitErr(err); !isExit || code != 1 {
			return "", fmt.Errorf("git diff HEAD %s: %w", branch, err)
		}
		return "branch " + branch + " has changes not in HEAD yet: commit the landed files first", nil
	}
	return "", nil
}

// QueuedLane is a lane the lane-land hook could not land by itself: a to-do the main session works off.
type QueuedLane struct {
	Lane     string `json:"lane"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch,omitempty"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail,omitempty"`
	Task     string `json:"task,omitempty"`
	Next     string `json:"next"`
	QueuedAt int64  `json:"queued_at"`
}

type landQueue struct {
	Schema int          `json:"schema"`
	Lanes  []QueuedLane `json:"lanes"`
}

const landQueueSchema = 1

var _ = schemas.Register(schemas.Format{
	Name: "land-queue", Owner: "internal/worktree", Kind: schemas.Record, Version: landQueueSchema,
	Glob: LandQueueFile, New: func() any { return &landQueue{} },
	Keep: "one entry per lane; a land or the lane's removal drops it",
})

// LandQueueFile is the one queue file every session reads.
func LandQueueFile() string {
	dir := filepath.Join(paths.StateDir(), "worktree")
	return paths.MoveLegacy(filepath.Join(dir, "merge-queue.json"), filepath.Join(dir, "land-queue.json"))
}

// Enqueue adds q or replaces the entry for its lane; a zero QueuedAt keeps the first queued-at, a set one wins.
func Enqueue(q QueuedLane) error { return EnqueueWithin(queueEditWait, q) }

// EnqueueWithin is Enqueue with the queue lock wait bounded by wait.
func EnqueueWithin(wait time.Duration, q QueuedLane) error {
	q.Lane = paths.Canon(q.Lane)
	keep := q.QueuedAt == 0
	if keep {
		q.QueuedAt = time.Now().Unix()
	}
	return editQueueWithin(wait, func(lanes []QueuedLane) []QueuedLane {
		if i := slices.IndexFunc(lanes, func(e QueuedLane) bool { return e.Lane == q.Lane }); i >= 0 {
			if keep {
				q.QueuedAt = lanes[i].QueuedAt
			}
			return slices.Concat(lanes[:i], []QueuedLane{q}, lanes[i+1:])
		}
		return append(slices.Clone(lanes), q)
	})
}

// Dequeue drops lane's entry; a missing queue or entry is no error.
func Dequeue(lane string) error {
	lane = paths.Canon(lane)
	return editQueue(func(lanes []QueuedLane) []QueuedLane {
		return slices.DeleteFunc(slices.Clone(lanes), func(e QueuedLane) bool { return e.Lane == lane })
	})
}

// Queued lists main's queued lanes, dropping (and persisting the drop of) entries whose lane no longer exists.
func Queued(main string) []QueuedLane { return queuedLanes(main, true) }

// QueuedReadOnly is Queued without the prune: it never writes the queue file, so a dry run changes nothing.
func QueuedReadOnly(main string) []QueuedLane { return queuedLanes(main, false) }

func queuedLanes(main string, prune bool) []QueuedLane {
	q, err := readQueue(LandQueueFile())
	if err != nil || len(q.Lanes) == 0 {
		return nil
	}
	var out []QueuedLane
	gone := false
	for _, e := range q.Lanes {
		if !paths.IsDir(e.Lane) {
			gone = true
		} else if main == "" || paths.SamePath(e.Repo, main) {
			out = append(out, e)
		}
	}
	if gone && prune { // best effort: a busy lock leaves the prune to the next read, so a prompt hook never waits on it
		_ = editQueueWithin(queuePruneWait, func(lanes []QueuedLane) []QueuedLane {
			return slices.DeleteFunc(slices.Clone(lanes), func(e QueuedLane) bool { return !paths.IsDir(e.Lane) })
		})
	}
	return out
}

// QueuedFor is lane's entry in main's queue, if any.
func QueuedFor(main, lane string) (QueuedLane, bool) {
	lanes := Queued(main)
	i := slices.IndexFunc(lanes, func(e QueuedLane) bool { return paths.SamePath(e.Lane, lane) })
	if i < 0 {
		return QueuedLane{}, false
	}
	return lanes[i], true
}

// queuePruneWait bounds how long a reader waits to drop removed lanes; queueEditWait bounds a writer.
const (
	queuePruneWait = 100 * time.Millisecond
	queueEditWait  = 10 * time.Second
)

func editQueue(fn func([]QueuedLane) []QueuedLane) error { return editQueueWithin(queueEditWait, fn) }

func editQueueWithin(wait time.Duration, fn func([]QueuedLane) []QueuedLane) error {
	file := LandQueueFile()
	return paths.WithFileLockWithin(file, wait, func() error {
		q, err := readQueue(file)
		if err != nil {
			return err
		}
		next := fn(q.Lanes)
		if len(next) == 0 {
			if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		return paths.WriteJSON(file, landQueue{Schema: landQueueSchema, Lanes: next})
	})
}

func readQueue(file string) (landQueue, error) {
	var q landQueue
	raw, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return q, nil
	}
	if err == nil {
		err = json.Unmarshal(raw, &q)
	}
	return q, err
}

// landRunName keys the one lease a land holds per checkout; every session's land queues on it.
const landRunName = "(land-run)"

// withLandRun runs fn as the only land of repo, waiting in line behind another; fn gets how long it waited.
func withLandRun(ctx context.Context, repo *Repo, lane string, out io.Writer, fn func(waited time.Duration) error) error {
	lease := operationLeasePath(repo.Main, landRunName)
	if err := os.MkdirAll(filepath.Dir(lease), 0o755); err != nil {
		return err
	}
	who := cmp.Or(os.Getenv(LandWaitEnv), "manual land")
	start, announced := nowFn(), false
	for {
		ran, err := paths.TryFileLock(lease, 0, func() error {
			_ = paths.AtomicWrite(lease+".who", []byte(strconv.Itoa(os.Getpid())+"\n"+who+"\n"), 0o600)
			return fn(nowFn().Sub(start))
		})
		if err != nil || ran {
			return err
		}
		pid, by := leaseHolder(lease + ".who")
		if !announced {
			fmt.Fprintf(out, "land %s: another land is running (pid %s, %s); waiting in line\n", lane, pid, by)
			announced = true
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if nowFn().Sub(start) > maxLandWait {
			return fmt.Errorf("worktree land: another land is running (pid %s, %s) after waiting %s", pid, by, maxLandWait)
		}
		sleepFn(pollInterval)
	}
}

var (
	gateSlotWait = 10 * time.Minute
	gateSlotPoll = 15 * time.Second
)

// HeldGate is a warm slot held for a land's Go gate; a nil *HeldGate means gate in the lane.
type HeldGate struct {
	dir  string
	slot *Slot
}

// usesGateSlot reports whether the gate is a go build/vet/test gate that benefits from a warm slot.
func usesGateSlot(gate, source string) bool {
	return gate == fullGoGate || strings.HasPrefix(source, scopedGateSource)
}

// HoldGate acquires a slot at oid, waiting up to 10 min while all are busy; nil on any failure (gate in the lane).
func HoldGate(ctx context.Context, top, oid, lane string, out io.Writer) *HeldGate {
	deadline := time.Now().Add(gateSlotWait)
	for warned := false; ; warned = true {
		s, err := TryAcquireSlot(ctx, top, oid)
		if err != nil {
			fmt.Fprintf(out, "land %s: no gate slot (%v); gating in the lane\n", lane, err)
			return nil
		}
		if s != nil {
			return &HeldGate{dir: s.Dir, slot: s}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil
		}
		if !warned {
			fmt.Fprintf(out, "land %s: waiting for a gate slot (%d busy)\n", lane, slotCount)
		}
		select {
		case <-ctx.Done():
		case <-time.After(gateSlotPoll):
		}
	}
}

// Where is the dir to gate in: the slot, or laneDir when nothing is held.
func (h *HeldGate) Where(laneDir string) string {
	if h == nil {
		return laneDir
	}
	return h.dir
}

// At is where the plan's gate runs: the slot, or the lane when nothing is held.
func (h *HeldGate) At(plan *LandPlan) gateAt {
	return gateAt{Dir: h.Where(plan.WorktreePath), Base: plan.TargetOID, Lane: plan.WorktreePath}
}

// Reset moves a held slot to oid for the next revalidation round; a nil gate has nothing to reset.
func (h *HeldGate) Reset(ctx context.Context, oid string) error {
	if h == nil {
		return nil
	}
	return h.slot.Reset(ctx, oid)
}

// Release hands the slot back; safe on nil.
func (h *HeldGate) Release() {
	if h != nil {
		h.slot.Release()
	}
}
