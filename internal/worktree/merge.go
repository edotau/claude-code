package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// MergeResult reports one terminal transition of the two-phase merge — "integrate" (inside the
// worktree) or "promote" (the fast-forward onto repo.Main). ResumeHint is populated only when the
// caller must retry manually (the target moved, or ancestry no longer holds).
type MergeResult struct {
	Phase      string
	BeforeOID  string
	AfterOID   string
	ResumeHint string
}

// integrateTarget merges the pinned target OID into the worktree itself — never repo.Main — so any
// integration commit lands in the worktree's own history. Callers gate on plan.Issues (a predicted
// conflict) before calling this; resolve handles the ones that stop here instead.
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
	out, err := runGit(ctx, plan.WorktreePath, nil, "merge", "--no-edit", plan.TargetOID)
	if err != nil {
		return nil, fmt.Errorf("git merge inside worktree: %w (%s)", err, firstNonEmpty(gitErrorDetail(err), string(out)))
	}
	after, err := headOfContext(ctx, plan.WorktreePath)
	if err != nil {
		return nil, err
	}
	plan.IntegratedOID = after // pins exactly what promote is allowed to fast-forward onto
	return &MergeResult{Phase: "integrate", BeforeOID: before, AfterOID: after}, nil
}

// promote reacquires the merge lock, rechecks both OIDs and checkout states against what Preflight
// pinned, requires ancestry, and fast-forwards repo.Main onto the integrated worktree HEAD — never a
// real merge commit. Target movement or a failed ancestry/re-simulation check stops with a retry hint;
// this never falls back to a real conflicting merge. A ctx already canceled when promote is entered
// returns immediately without acquiring the lock or touching repo.Main.
func promote(ctx context.Context, repo *Repo, plan *MergePlan) (*MergeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result *MergeResult
	err := acquire(0, func() error {
		r, lockedErr := promoteLocked(ctx, repo, plan)
		result = r
		return lockedErr
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
		return nil, err // never start the ff-only update once canceled — repo.Main stays untouched
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

// MergeOutcome is a finished merge: Blocked means the plan was refused (already shown); Clean is nil when NoClean.
type MergeOutcome struct {
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

// Merge runs the non-dry-run lifecycle under the caller's worktree lease: resume-or-plan, the verify-gate decision,
// then integrate -> verify -> promote -> clean. Errors are unprefixed; the caller adds "worktree merge: ".
func Merge(ctx context.Context, repo *Repo, req MergeRequest) (*MergeOutcome, error) {
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
		return &MergeOutcome{Blocked: true}, nil
	}
	gate, gateSource, err := chooseVerifyGate(plan.WorktreePath, req.Verify, req.NoVerify)
	if err != nil {
		return nil, err
	}
	return applyMerge(ctx, repo, plan, req, gate, gateSource)
}

// applyMerge runs integrateTarget -> the gate (if any) -> promote -> an optional clean, inside the caller's lease.
func applyMerge(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest, gate, gateSource string) (*MergeOutcome, error) {
	integrated, err := integrateTarget(ctx, repo, plan)
	if err != nil {
		return nil, fmt.Errorf("integrate: %w", err)
	}
	fmt.Fprintf(req.Out, "integrated: %s -> %s\n", ShortOID(integrated.BeforeOID), ShortOID(integrated.AfterOID))

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

// scopeGate narrows a Makefile test gate to the changed Go packages unless FullVerify; other gates pass through.
func scopeGate(ctx context.Context, plan *MergePlan, req MergeRequest, gate, gateSource string) (g, source string, err error) {
	if gateSource != "Makefile" || req.FullVerify || !paths.FileExists(filepath.Join(plan.WorktreePath, "go.mod")) {
		return gate, gateSource, nil
	}
	scoped, pkgs, full, err := scopedGoGate(ctx, plan.WorktreePath, plan.TargetOID)
	if err != nil {
		return "", "", fmt.Errorf("scoped gate: %w", err)
	}
	if full {
		return scoped, "go.mod changed, all packages", nil
	}
	return scoped, "scoped go gate, " + strconv.Itoa(len(pkgs)) + " test pkgs", nil
}

// afterPromote reports the promotion, ticks the plan, then cleans the worktree unless NoClean. The ledger closes
// exactly once: by the clean (carrying the merge and branch state) or, when no removal happens, right here.
func afterPromote(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest, promoted, integrated *MergeResult) (*MergeOutcome, error) {
	fmt.Fprintf(req.Out, "promoted:   %s -> %s on %s\n", ShortOID(promoted.BeforeOID), ShortOID(promoted.AfterOID), plan.TargetRef)
	if req.AfterPromote != nil {
		if err := req.AfterPromote(ctx, promoted.BeforeOID+".."+promoted.AfterOID); err != nil {
			fmt.Fprintf(req.Err, "worktree merge: warning: plan tick: %v\n", err)
		}
	}
	closing := LedgerClose{Via: "merge", MergeSHA: promoted.AfterOID, Head: integrated.AfterOID}
	if req.NoClean {
		warnLedger(req.Err, CloseLedger(repo.Main, req.Name, closing))
		return &MergeOutcome{}, nil
	}
	result, err := cleanAtWith(repo, req.Name, plan.WorktreePath, false, req.Err, closing)
	if !result.Removed {
		warnLedger(req.Err, CloseLedger(repo.Main, req.Name, closing)) // refused or failed: the merge still landed
	}
	if err != nil {
		return nil, fmt.Errorf("clean: %w", err)
	}
	return &MergeOutcome{Clean: &result}, nil
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
	fmt.Fprintf(req.Out, "verify: %s\n", gate)
	if err := runVerify(ctx, plan.WorktreePath, gate, req.Out, req.Err); err != nil {
		fmt.Fprintf(req.Err, "worktree left on disk at %s for inspection\n", plan.WorktreePath)
		return fmt.Errorf("verify failed, not promoting: %w", err)
	}
	fmt.Fprintf(req.Out, "verify: passed (%s)\n", gateSource)
	return nil
}

// PlanOrResolve runs Preflight and shows the plan; a predicted conflict with ResolveAgent set goes to ResolveWithAgent
// and re-Preflights. A nil plan with a nil error means blocked and already shown: dry-run, no agent, or a non-conflict Issue.
func PlanOrResolve(ctx context.Context, repo *Repo, req MergeRequest, dryRun bool) (*MergePlan, error) {
	plan, err := Preflight(ctx, repo, req.Name, req.Target)
	if err != nil {
		return nil, err
	}
	req.ShowPlan(plan)
	if len(plan.Issues) == 0 {
		return plan, nil
	}
	if dryRun || !onlyConflictIssue(plan.Issues) {
		return nil, nil
	}
	if req.ResolveAgent == "" {
		notifyConflict(plan, req.Err)
		return nil, nil
	}

	return resolveConflict(ctx, repo, plan, req)
}

// resolveConflict hands the conflicted plan to the resolve agent, then re-plans and requires the target unmoved.
func resolveConflict(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest) (*MergePlan, error) {
	origTargetRef, origTargetOID := plan.TargetRef, plan.TargetOID
	_, result, err := ResolveWithAgent(ctx, repo, plan, req.ResolveAgent, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	req.ShowResolved(result)
	return replanPinned(ctx, repo, req, origTargetRef, origTargetOID, "agent resolution", "during resolution")
}

// resumeMerge reattaches to the --resolve-agent operation saved for req.Name without re-invoking the agent, then
// returns a fresh Preflight plan; both the saved target branch and OID must still match.
func resumeMerge(ctx context.Context, repo *Repo, req MergeRequest) (*MergePlan, error) {
	op, worktreePath, err := loadResumable(repo, req.Name)
	if err != nil {
		return nil, err
	}
	plan := &MergePlan{WorktreePath: worktreePath}
	_, result, err := ResolveWithAgent(ctx, repo, plan, op.Agent, op)
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

// notifyConflict appends a conflict notice to the session owning the worktree's ledger; a failure only warns.
func notifyConflict(plan *MergePlan, errOut io.Writer) {
	s := ledgerSession(plan.WorktreePath)
	if s == "" {
		return
	}
	var files []string
	for _, iss := range plan.Issues {
		files = append(files, iss.Paths...)
	}
	err := Append(s, Notice{TS: time.Now().Unix(), Kind: "conflict", From: "worktree merge",
		Text: fmt.Sprintf("merge of %s into %s predicts a conflict in %s; rebase it or run "+
			"`claude-code worktree merge --resolve-agent codex %s`", plan.WorktreePath, plan.TargetRef,
			strings.Join(files, ", "), filepath.Base(plan.WorktreePath))})
	if err != nil && errOut != nil {
		fmt.Fprintf(errOut, "worktree merge: warning: conflict notice: %v\n", err)
	}
}

// onlyConflictIssue reports whether every blocking Issue is a predicted conflict, the one case --resolve-agent acts on.
func onlyConflictIssue(issues []Issue) bool {
	if len(issues) == 0 {
		return false
	}
	for _, iss := range issues {
		if iss.Code != IssueConflict {
			return false
		}
	}
	return true
}

func issueCodes(issues []Issue) string {
	codes := make([]string, len(issues))
	for i, iss := range issues {
		codes[i] = iss.Code
	}
	return strings.Join(codes, ", ")
}

// mergeLockFile is the one harness lock serializing the fast-forward promote step across concurrent
// `worktree merge` runs. It coordinates this harness's own operations only, never an external writer.
const mergeLockFile = "worktree-merge.lock"

// mergeLockWait is how long acquire waits for the lock (when wait<=0) before errMergeBusy; a var so tests need not sit it out.
var mergeLockWait = 5 * time.Second

// errMergeBusy is returned when the merge lock is not granted within the wait.
var errMergeBusy = errors.New("worktree merge: another promote holds the merge lock")

// acquire runs fn holding the one merge lock; wait<=0 means mergeLockWait. Not reentrant: fn must
// never call acquire itself, or a single caller would deadlock on its own held lock.
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
