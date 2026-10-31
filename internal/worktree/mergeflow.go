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
	gate, gateSource, err := ChooseVerifyGate(plan.WorktreePath, req.Verify, req.NoVerify)
	if err != nil {
		return nil, err
	}
	return applyMerge(ctx, repo, plan, req, gate, gateSource)
}

// applyMerge runs IntegrateTarget -> the gate (if any) -> Promote -> an optional clean, inside the caller's lease.
func applyMerge(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest, gate, gateSource string) (*MergeOutcome, error) {
	integrated, err := IntegrateTarget(ctx, repo, plan)
	if err != nil {
		return nil, fmt.Errorf("integrate: %w", err)
	}
	fmt.Fprintf(req.Out, "integrated: %s -> %s\n", ShortOID(integrated.BeforeOID), ShortOID(integrated.AfterOID))

	if gateSource == "Makefile" && !req.FullVerify && paths.FileExists(filepath.Join(plan.WorktreePath, "go.mod")) {
		scoped, pkgs, full, err := ScopedGoGate(ctx, plan.WorktreePath, plan.TargetOID)
		if err != nil {
			return nil, fmt.Errorf("scoped gate: %w", err)
		}
		gate, gateSource = scoped, "scoped go gate, "+strconv.Itoa(len(pkgs))+" test pkgs"
		if full {
			gateSource = "go.mod changed, all packages"
		}
	}
	if err := runGate(ctx, plan, req, gate, gateSource); err != nil {
		return nil, err
	}

	promoted, err := Promote(ctx, repo, plan)
	if err != nil {
		if promoted != nil && promoted.ResumeHint != "" {
			fmt.Fprintln(req.Err, "  retry:", promoted.ResumeHint)
		}
		return nil, fmt.Errorf("promote: %w", err)
	}
	fmt.Fprintf(req.Out, "promoted:   %s -> %s on %s\n", ShortOID(promoted.BeforeOID), ShortOID(promoted.AfterOID), plan.TargetRef)
	if req.AfterPromote != nil {
		if err := req.AfterPromote(ctx, promoted.BeforeOID+".."+promoted.AfterOID); err != nil {
			fmt.Fprintf(req.Err, "worktree merge: warning: plan tick: %v\n", err)
		}
	}
	if err := CloseLedger(repo.Main, req.Name, LedgerClose{Via: "merge", MergeSHA: promoted.AfterOID, Head: integrated.AfterOID}); err != nil {
		fmt.Fprintf(req.Err, "worktree merge: warning: ledger: %v\n", err)
	}
	if req.NoClean {
		return &MergeOutcome{}, nil
	}
	result, err := CleanInLease(repo, req.Name, false, req.Err)
	if err != nil {
		return nil, fmt.Errorf("clean: %w", err)
	}
	return &MergeOutcome{Clean: &result}, nil
}

// runGate runs the verify gate (or announces its absence); a failure leaves the worktree on disk and blocks promotion.
func runGate(ctx context.Context, plan *MergePlan, req MergeRequest, gate, gateSource string) error {
	if gate == "" {
		fmt.Fprintln(req.Out, "verify: skipped (--no-verify) — promoting unverified")
		return nil
	}
	fmt.Fprintf(req.Out, "verify: %s\n", gate)
	if err := RunVerify(ctx, plan.WorktreePath, gate, req.Out, req.Err); err != nil {
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
	if dryRun || req.ResolveAgent == "" || !onlyConflictIssue(plan.Issues) {
		return nil, nil
	}

	origTargetRef, origTargetOID := plan.TargetRef, plan.TargetOID
	_, result, err := ResolveWithAgent(ctx, repo, plan, req.ResolveAgent, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	req.ShowResolved(result)

	plan, err = PreflightAfterResolution(ctx, repo, req.Name, req.Target) // the resolution is committed now
	if err != nil {
		return nil, err
	}
	if len(plan.Issues) > 0 {
		req.ShowPlan(plan)
		return nil, fmt.Errorf("still blocked after agent resolution (%s)", issueCodes(plan.Issues))
	}
	if plan.TargetRef != origTargetRef || plan.TargetOID != origTargetOID {
		return nil, fmt.Errorf("target moved from %s (%s) to %s (%s) during resolution — re-run worktree merge explicitly to retry against the new target",
			origTargetRef, ShortOID(origTargetOID), plan.TargetRef, ShortOID(plan.TargetOID))
	}
	return plan, nil
}

// resumeMerge reattaches to the --resolve-agent operation saved for req.Name without re-invoking the agent, then
// returns a fresh Preflight plan; both the saved target branch and OID must still match.
func resumeMerge(ctx context.Context, repo *Repo, req MergeRequest) (*MergePlan, error) {
	op, err := LoadOperation(repo, req.Name)
	if err != nil {
		return nil, fmt.Errorf("no in-progress --resolve-agent operation found for %q: %w", req.Name, err)
	}
	if op.Repo != repo.Main {
		return nil, fmt.Errorf("operation %s belongs to %s, not %s", op.ID, op.Repo, repo.Main)
	}
	worktreePath := filepath.Join(repo.Root, op.Worktree)
	if _, err := os.Stat(worktreePath); err != nil {
		return nil, fmt.Errorf("worktree %q not found at %s: %w", op.Worktree, worktreePath, err)
	}
	plan := &MergePlan{WorktreePath: worktreePath}
	_, result, err := ResolveWithAgent(ctx, repo, plan, op.Agent, op)
	if err != nil {
		return nil, err
	}
	req.ShowResolved(result)
	fresh, err := PreflightAfterResolution(ctx, repo, req.Name, req.Target)
	if err != nil {
		return nil, err
	}
	if len(fresh.Issues) > 0 {
		req.ShowPlan(fresh)
		return nil, fmt.Errorf("still blocked after resuming resolution (%s)", issueCodes(fresh.Issues))
	}
	if fresh.TargetRef != op.TargetRef || fresh.TargetOID != op.TargetOID {
		return nil, fmt.Errorf("target moved from %s (%s) to %s (%s) since the operation started — re-run worktree merge explicitly to retry against the new target",
			op.TargetRef, ShortOID(op.TargetOID), fresh.TargetRef, ShortOID(fresh.TargetOID))
	}
	return fresh, nil
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

// MergeLockFile is the one harness lock serializing the fast-forward promote step across concurrent
// `worktree merge` runs. It coordinates this harness's own operations only, never an external writer.
const MergeLockFile = "worktree-merge.lock"

// MergeLockWait bounds how long Acquire waits for the lock before giving up with ErrMergeBusy.
const MergeLockWait = 5 * time.Second

// mergeLockWait is the wait<=0 default; a var so tests need not sit out the full MergeLockWait.
var mergeLockWait = MergeLockWait

// ErrMergeBusy is returned when the merge lock is not granted within the wait.
var ErrMergeBusy = errors.New("worktree merge: another promote holds the merge lock")

// MergeLockPath is the physical lock file under paths.StateDir().
func MergeLockPath() string { return filepath.Join(paths.StateDir(), MergeLockFile) }

// Acquire runs fn holding the one merge lock; wait<=0 means MergeLockWait. Not reentrant: fn must
// never call Acquire itself, or a single caller would deadlock on its own held lock.
func Acquire(wait time.Duration, fn func() error) error {
	dir := paths.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if wait <= 0 {
		wait = mergeLockWait
	}
	ran, err := paths.TryFileLock(MergeLockPath(), wait, fn)
	if err == nil && !ran {
		return ErrMergeBusy
	}
	return err
}
