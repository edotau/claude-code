package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// ErrRebaseConflict marks a rebase that conflicted and was aborted, leaving the lane as it was.
var ErrRebaseConflict = errors.New("rebase conflict")

// MergeResult reports one terminal transition ("integrate" or "promote"); ResumeHint is set when the caller must retry.
type MergeResult struct {
	Phase      string
	BeforeOID  string
	AfterOID   string
	ResumeHint string
}

// integrateTarget rebases the lane onto the pinned target inside the lane (never repo.Main); a lane on it is kept.
func integrateTarget(ctx context.Context, repo *Repo, plan *MergePlan) (*MergeResult, error) {
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
		return &MergeResult{Phase: "integrate", BeforeOID: before, AfterOID: before}, nil
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
	return &MergeResult{Phase: "integrate", BeforeOID: before, AfterOID: after}, nil
}

// abortRebase undoes a failed rebase so the lane is exactly as before; a conflict records and names its files.
func abortRebase(ctx context.Context, plan *MergePlan, before string, out []byte, cause error) error {
	failed := fmt.Errorf("git rebase inside lane: %w (%s)", cause, firstNonEmpty(gitErrorDetail(cause), string(out)))
	op, pending, err := inProgressOp(ctx, plan.WorktreePath, plan.WorktreeGitDir)
	if err != nil || !pending || op != "rebase" {
		return errors.Join(failed, err)
	}
	conflicts, listErr := runGit(ctx, plan.WorktreePath, nil, "diff", "--name-only", "--diff-filter=U", "-z")
	// a canceled merge must still abort, so the abort gets its own timeout
	abortCtx, cancel := gitTimeoutCtx()
	defer cancel()
	if abortOut, err := runGit(abortCtx, plan.WorktreePath, nil, "rebase", "--abort"); err != nil {
		return errors.Join(failed, fmt.Errorf("git rebase --abort in %s failed, abort by hand: %w (%s)",
			plan.WorktreePath, err, firstNonEmpty(gitErrorDetail(err), string(abortOut))))
	}
	files := strings.FieldsFunc(string(conflicts), func(r rune) bool { return r == '\x00' })
	if listErr != nil || len(files) == 0 {
		return errors.Join(fmt.Errorf("%w; rebase aborted", failed), listErr)
	}
	plan.Issues = []Issue{{Code: IssueConflict, Paths: files}}
	return fmt.Errorf("%w in %s: %s; rebase aborted, lane left at %s; resolve it in the lane "+
		"(--resolve-agent <agent> or a fix leaf), then re-run worktree merge", ErrRebaseConflict, plan.WorktreePath,
		strings.Join(files, ", "), ShortOID(before))
}

// promote rechecks OIDs and states under the merge lock, requires ancestry and fast-forwards repo.Main.
func promote(ctx context.Context, repo *Repo, plan *MergePlan) (*MergeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result *MergeResult
	err := acquire(0, func() (err error) {
		result, err = promoteLocked(ctx, repo, plan)
		return err
	})
	return result, err
}

func promoteLocked(ctx context.Context, repo *Repo, plan *MergePlan) (*MergeResult, error) {
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
	// promoteFreshHeads proved ancestry, so the ff-only merge below cannot conflict: no re-simulation.
	if err := ctx.Err(); err != nil {
		// never start the ff-only update once canceled — repo.Main stays untouched
		return nil, err
	}
	out, err := runGit(ctx, repo.Main, nil, "-c", "submodule.recurse=false", "merge", "--ff-only", "--no-autostash", freshSource)
	if err != nil {
		return nil, fmt.Errorf("git merge --ff-only: %w (%s)", err, firstNonEmpty(gitErrorDetail(err), string(out)))
	}
	return &MergeResult{Phase: "promote", BeforeOID: freshTarget, AfterOID: freshSource}, nil
}

// promoteStateChecks requires the pinned target, no overlapping target edits, and a clean source worktree.
func promoteStateChecks(ctx context.Context, repo *Repo, plan *MergePlan) error {
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
		return fmt.Errorf("promote: %s: %v", issue.Code, issue.Detail)
	}
	if issue, err := checkWorktreeState(ctx, plan.WorktreePath, plan.WorktreeGitDir, plan.AfterResolution); err != nil {
		return err
	} else if issue != nil {
		return fmt.Errorf("promote: %s: %v", issue.Code, issue.Detail)
	}
	return nil
}

// promoteFreshHeads re-reads both heads and requires them to match the pinned integration and a fast-forward.
func promoteFreshHeads(ctx context.Context, repo *Repo, plan *MergePlan) (target, source string, res *MergeResult, err error) {
	freshTarget, err := headOfContext(ctx, repo.Main)
	if err != nil {
		return "", "", nil, err
	}
	freshSource, err := headOfContext(ctx, plan.WorktreePath)
	if err != nil {
		return "", "", nil, err
	}
	if freshTarget != plan.TargetOID {
		return "", "", &MergeResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "target moved since integration — re-run worktree merge to re-integrate and retry"},
			fmt.Errorf("promote: target moved from %s to %s since integration", plan.TargetOID, freshTarget)
	}
	if freshSource != plan.IntegratedOID {
		return "", "", &MergeResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "worktree changed since the verified integration — re-run worktree merge to re-integrate and retry"},
			fmt.Errorf("promote: worktree HEAD moved from the verified integration %s to %s since integration — refusing to promote an unreviewed commit",
				plan.IntegratedOID, freshSource)
	}

	ancestor, err := isAncestor(ctx, repo.Main, freshTarget, freshSource)
	if err != nil {
		return "", "", nil, fmt.Errorf("promote: %w", err)
	}
	if !ancestor {
		return "", "", &MergeResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "not a fast-forward — re-run worktree merge to re-integrate and retry"},
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

// MergeRequest is one `worktree merge` apply; Out/Err get the progress lines, the Show* hooks render plans and results.
type MergeRequest struct {
	Name, Target, ResolveAgent, Verify    string
	Resume, NoClean, FullVerify, NoVerify bool
	Out, Err                              io.Writer
	ShowPlan                              func(*MergePlan)
	ShowResolved                          func(*MergeResult)
	AfterPromote                          func(ctx context.Context, commitRange string) error
}

// mergeOutcome is a finished merge: Blocked means the plan was refused (already shown); Clean is nil when NoClean.
type mergeOutcome struct {
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

// Merge runs the lifecycle under the caller's lease: plan or resume, gate, rebase, verify, promote, notify, clean.
func Merge(ctx context.Context, repo *Repo, req MergeRequest) (*mergeOutcome, error) {
	defer BeginScanMemo()()
	var plan *MergePlan
	var err error
	if req.Resume {
		plan, err = resumeMerge(ctx, repo, req)
	} else {
		plan, err = PlanOrResolve(ctx, repo, req, false)
	}
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return &mergeOutcome{Blocked: true}, nil
	}
	gate, gateSource, err := chooseVerifyGate(plan.WorktreePath, req.Verify, req.NoVerify)
	if err != nil {
		return nil, err
	}
	return applyMerge(ctx, repo, plan, req, gate, gateSource)
}

// applyMerge runs the lane rebase -> the gate (if any) -> promote -> an optional clean, inside the caller's lease.
func applyMerge(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest, gate, gateSource string) (*mergeOutcome, error) {
	plan, integrated, err := integrateLane(ctx, repo, plan, req)
	if err != nil {
		return nil, fmt.Errorf("integrate: %w", err)
	}
	if integrated.BeforeOID == integrated.AfterOID {
		fmt.Fprintf(req.Out, "rebase:     skipped, lane already on %s\n", plan.TargetRef)
	} else {
		fmt.Fprintf(req.Out, "rebased:    %s -> %s in %s\n", ShortOID(integrated.BeforeOID), ShortOID(integrated.AfterOID), plan.WorktreePath)
	}

	gate, gateSource, err = scopeGate(ctx, plan, req, gate, gateSource)
	if err != nil {
		return nil, err
	}
	if err := runGate(ctx, plan, req, gate, gateSource); err != nil {
		return nil, err
	}

	promoted, err := promote(ctx, repo, plan)
	if err != nil {
		if promoted != nil && promoted.ResumeHint != "" {
			fmt.Fprintln(req.Err, "  retry:", promoted.ResumeHint)
		}
		return nil, fmt.Errorf("promote: %w", err)
	}
	return afterPromote(ctx, repo, plan, req, promoted, integrated)
}

// scopeGate narrows a Makefile test gate to the changed Go packages unless FullVerify or the whole suite is due.
func scopeGate(ctx context.Context, plan *MergePlan, req MergeRequest, gate, gateSource string) (g, source string, err error) {
	if gateSource != "Makefile" || req.FullVerify || !paths.FileExists(filepath.Join(plan.WorktreePath, "go.mod")) {
		return gate, gateSource, nil
	}
	scoped, pkgs, nChanged, full, err := scopedGoGate(ctx, plan.WorktreePath, plan.TargetOID)
	if err != nil {
		return "", "", fmt.Errorf("scoped gate: %w", err)
	}
	if full {
		// the whole suite is the Makefile gate itself, not a hard-coded go test
		return gate, gateSource, nil
	}
	return scoped, fmt.Sprintf("%s, tests: %d changed + %d direct importers (full suite runs in the post-merge audit)",
		scopedGateSource, nChanged, len(pkgs)-nChanged), nil
}

// afterPromote reports the promotion, ticks the plan, then cleans unless NoClean; clean closes the ledger once.
func afterPromote(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest, promoted, integrated *MergeResult) (*mergeOutcome, error) {
	fmt.Fprintf(req.Out, "promoted:   %s -> %s on %s\n", ShortOID(promoted.BeforeOID), ShortOID(promoted.AfterOID), plan.TargetRef)
	if req.AfterPromote != nil {
		if err := req.AfterPromote(ctx, promoted.BeforeOID+".."+promoted.AfterOID); err != nil {
			fmt.Fprintf(req.Err, "worktree merge: warning: plan tick: %v\n", err)
		}
	}
	notifyLanes(ctx, repo, req, plan, promoted)
	if line := StartAudit(repo.Main, promoted.AfterOID); line != "" {
		fmt.Fprintln(req.Out, line)
	}
	if req.NoClean {
		// the lane stays in use by design: its ledger stays open so it keeps getting notices until clean
		return &mergeOutcome{}, nil
	}
	closing := LedgerClose{Via: "merge", MergeSHA: promoted.AfterOID, Head: integrated.AfterOID}
	result, err := cleanAtWith(repo, req.Name, plan.WorktreePath, false, req.Err, closing)
	if !result.Removed {
		// refused or failed: the merge still landed
		warnLedger(req.Err, CloseLedger(repo.Main, req.Name, closing))
	}
	if err != nil {
		return nil, fmt.Errorf("clean: %w", err)
	}
	return &mergeOutcome{Clean: &result}, nil
}

// warnLedger reports a failed ledger close as a warning; the merge itself already landed.
func warnLedger(errOut io.Writer, err error) {
	if err != nil {
		fmt.Fprintf(errOut, "worktree merge: warning: ledger: %v\n", err)
	}
}

// runGate runs the verify gate (or announces its absence); a failure leaves the worktree on disk and blocks promotion.
func runGate(ctx context.Context, plan *MergePlan, req MergeRequest, gate, gateSource string) error {
	if gate == "" {
		fmt.Fprintln(req.Out, "verify: skipped (--no-verify) — promoting unverified")
		return nil
	}
	steps := gateSteps(gate, gateSource)
	fmt.Fprintf(req.Out, "verify: %s\n", gate)
	if len(steps) > 1 {
		fmt.Fprintln(req.Out, "verify: build, vet and tests run concurrently")
	}
	if err := runSteps(ctx, plan.WorktreePath, steps, plan.TargetOID, req.Out, req.Err); err != nil {
		fmt.Fprintf(req.Err, "worktree left on disk at %s for inspection\n", plan.WorktreePath)
		return fmt.Errorf("verify failed, not promoting: %w", err)
	}
	fmt.Fprintf(req.Out, "verify: passed (%s)\n", gateSource)
	return nil
}

// PlanOrResolve runs Preflight; a predicted conflict proceeds to the lane rebase; nil plan = blocked.
func PlanOrResolve(ctx context.Context, repo *Repo, req MergeRequest, dryRun bool) (*MergePlan, error) {
	plan, err := Preflight(ctx, repo, req.Name, req.Target)
	if err != nil {
		return nil, err
	}
	req.ShowPlan(plan)
	if len(plan.Issues) > 0 && !onlyConflictIssue(plan.Issues) {
		return nil, nil
	}
	if dryRun {
		return plan, previewMerge(ctx, repo, plan, req)
	}
	return plan, nil
}

// integrateLane rebases the lane; a conflict notices its session, or with ResolveAgent is resolved in the lane.
func integrateLane(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest) (*MergePlan, *MergeResult, error) {
	result, err := integrateTarget(ctx, repo, plan)
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
	result, err = integrateTarget(ctx, repo, fresh)
	return fresh, result, err
}

// resolveConflict hands the conflicted plan to the resolve agent, then re-plans and requires the target unmoved.
func resolveConflict(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest) (*MergePlan, error) {
	origTargetRef, origTargetOID := plan.TargetRef, plan.TargetOID
	_, result, err := resolveWithAgent(ctx, repo, plan, req.ResolveAgent, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	req.ShowResolved(result)
	return replanPinned(ctx, repo, req, origTargetRef, origTargetOID, "agent resolution", "during resolution")
}

// resumeMerge reattaches to the saved --resolve-agent operation without re-invoking the agent and re-plans.
func resumeMerge(ctx context.Context, repo *Repo, req MergeRequest) (*MergePlan, error) {
	op, worktreePath, err := loadResumable(repo, req.Name)
	if err != nil {
		return nil, err
	}
	plan := &MergePlan{WorktreePath: worktreePath}
	_, result, err := resolveWithAgent(ctx, repo, plan, op.Agent, op)
	if err != nil {
		return nil, err
	}
	req.ShowResolved(result)
	return replanPinned(ctx, repo, req, op.TargetRef, op.TargetOID, "resuming resolution", "since the operation started")
}

// replanPinned re-plans a committed resolution, requiring it unblocked and the target still at the pinned ref and OID.
func replanPinned(ctx context.Context, repo *Repo, req MergeRequest, ref, oid, blockedAfter, movedWhen string) (*MergePlan, error) {
	fresh, err := preflightAfterResolution(ctx, repo, req.Name, req.Target)
	if err != nil {
		return nil, err
	}
	if len(fresh.Issues) > 0 {
		req.ShowPlan(fresh)
		return nil, fmt.Errorf("still blocked after %s (%s)", blockedAfter, issueCodes(fresh.Issues))
	}
	if fresh.TargetRef != ref || fresh.TargetOID != oid {
		return nil, fmt.Errorf("target moved from %s (%s) to %s (%s) %s — re-run worktree merge explicitly to retry against the new target",
			ref, ShortOID(oid), fresh.TargetRef, ShortOID(fresh.TargetOID), movedWhen)
	}
	return fresh, nil
}

// notifyConflict appends a conflict notice to the session owning the lane's ledger; a failure only warns.
func notifyConflict(plan *MergePlan, errOut io.Writer) {
	s := ledgerSession(plan.WorktreePath)
	if s == "" {
		return
	}
	err := Append(inboxFor(s, plan.WorktreePath), Notice{TS: time.Now().Unix(), Kind: "conflict", From: "worktree merge",
		Text: fmt.Sprintf("rebase of %s onto %s hit a conflict in %s and was aborted, lane unchanged; resolve it there "+
			"or run `claude-code worktree merge --resolve-agent codex %s`", plan.WorktreePath, plan.TargetRef,
			issuePaths(plan.Issues), filepath.Base(plan.WorktreePath))})
	if err != nil && errOut != nil {
		fmt.Fprintf(errOut, "worktree merge: warning: conflict notice: %v\n", err)
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

// mergeLockFile serializes the fast-forward promote across `worktree merge` runs, never external writers.
const mergeLockFile = "worktree-merge.lock"

// mergeLockWait is how long acquire waits (when wait<=0) before errMergeBusy; a var so tests need not sit it out.
var mergeLockWait = 5 * time.Second

// errMergeBusy is returned when the merge lock is not granted within the wait.
var errMergeBusy = errors.New("worktree merge: another promote holds the merge lock")

// acquire runs fn holding the merge lock (wait<=0 means mergeLockWait); not reentrant, fn must never call acquire.
func acquire(wait time.Duration, fn func() error) error {
	dir := paths.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if wait <= 0 {
		wait = mergeLockWait
	}
	ran, err := paths.TryFileLock(filepath.Join(dir, mergeLockFile), wait, fn)
	if err == nil && !ran {
		return errMergeBusy
	}
	return err
}

// previewMerge prints what the apply would do, in order, and the lane's divergence; it changes nothing.
func previewMerge(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest) error {
	verify := "skip (--no-verify), promote marked unverified"
	if gate, source, err := chooseVerifyGate(plan.WorktreePath, req.Verify, req.NoVerify); err != nil {
		verify = "refuse: " + err.Error()
	} else if gate != "" {
		verify = gate + " (" + source + ", scoped to changed Go packages unless --full-verify)"
	}
	clean := "remove the lane and its branch"
	if req.NoClean {
		clean = "keep the lane (--no-clean)"
	}
	fmt.Fprintf(req.Out, "would: rebase %s onto %s inside the lane; a conflict aborts it, lane unchanged (exit 3)\n",
		plan.WorktreePath, plan.TargetRef)
	fmt.Fprintf(req.Out, "would: verify in the lane: %s\n", verify)
	fmt.Fprintf(req.Out, "would: fast-forward %s only; tick Task: trailers; notice other lanes; %s\n", plan.TargetRef, clean)
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

// notifyLanes tells every other lane's session that trunk moved, naming the landed files it also changed.
func notifyLanes(ctx context.Context, repo *Repo, req MergeRequest, plan *MergePlan, landed *MergeResult) {
	lanes, err := listPorcelain(repo)
	if err != nil {
		warnNotice(req.Err, err)
		return
	}
	var touched map[string]bool
	for _, e := range lanes {
		if e.Branch == "" || e.Prunable || paths.SamePath(e.Path, plan.WorktreePath) {
			continue
		}
		session := listedSession(repo, e.Path)
		if session == "" {
			continue
		}
		if touched == nil {
			names, err := targetTouchedPaths(ctx, repo.Main, landed.BeforeOID+".."+landed.AfterOID)
			if err != nil {
				warnNotice(req.Err, err)
				return
			}
			touched = nameSet(names)
		}
		text := fmt.Sprintf("%s moved to %s (lane %s merged)", plan.TargetRef, ShortOID(landed.AfterOID), req.Name)
		warnNotice(req.Err, noticeLane(ctx, repo, e, inboxFor(session, e.Path), touched, landed.AfterOID, text))
	}
}

// noticeLane appends text to a lane behind sha, plus the files it changed or edits that the landed range touched.
func noticeLane(ctx context.Context, repo *Repo, e Entry, session string, touched map[string]bool, sha, text string) error {
	if current, err := isAncestor(ctx, repo.Main, sha, e.Head); err != nil || current {
		return err
	}
	changed, err := targetTouchedPaths(ctx, repo.Main, sha+"..."+e.Branch)
	if err != nil {
		return err
	}
	mine, err := targetDirtyPaths(ctx, e.Path)
	if err != nil {
		return err
	}
	for _, f := range changed {
		mine[f] = true
	}
	if shared := sharedFiles(touched, mine); len(shared) > 0 {
		text += "; overlaps yours in " + strings.Join(shared, ", ")
	}
	return Append(session, Notice{TS: time.Now().Unix(), Kind: "trunk-moved", From: "worktree merge", Text: text})
}

// warnNotice reports a failed lane notice as a warning; the merge itself already landed.
func warnNotice(errOut io.Writer, err error) {
	if err != nil {
		fmt.Fprintf(errOut, "worktree merge: warning: lane notice: %v\n", err)
	}
}
