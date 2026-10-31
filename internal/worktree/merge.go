package worktree

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// IntegrateTarget merges the pinned target OID into the worktree itself — never repo.Main — so any
// integration commit lands in the worktree's own history. Callers gate on plan.Issues (a predicted
// conflict) before calling this; resolve handles the ones that stop here instead.
func IntegrateTarget(ctx context.Context, repo *Repo, plan *MergePlan) (*MergeResult, error) {
	if issue, err := checkWorktreeState(ctx, plan.WorktreePath, plan.AfterResolution); err != nil {
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
	plan.IntegratedOID = after // pins exactly what Promote is allowed to fast-forward onto
	return &MergeResult{Phase: "integrate", BeforeOID: before, AfterOID: after}, nil
}

// Promote reacquires the merge lock, rechecks both OIDs and checkout states against what Preflight
// pinned, requires ancestry, and fast-forwards repo.Main onto the integrated worktree HEAD — never a
// real merge commit. Target movement or a failed ancestry/re-simulation check stops with a retry hint;
// this never falls back to a real conflicting merge. A ctx already canceled when Promote is entered
// returns immediately without acquiring the lock or touching repo.Main.
func Promote(ctx context.Context, repo *Repo, plan *MergePlan) (*MergeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result *MergeResult
	err := Acquire(0, func() error {
		r, lockedErr := promoteLocked(ctx, repo, plan)
		result = r
		return lockedErr
	})
	return result, err
}

func promoteLocked(ctx context.Context, repo *Repo, plan *MergePlan) (*MergeResult, error) {
	if plan.IntegratedOID == "" {
		return nil, errors.New("promote: no verified integration OID pinned — call IntegrateTarget first")
	}
	branch, err := requireBranch(ctx, repo.Main)
	if err != nil {
		return nil, fmt.Errorf("promote: %w", err)
	}
	if branch != plan.TargetRef {
		return nil, fmt.Errorf("promote: main checkout moved to %q, pinned target was %q — retry from a fresh plan", branch, plan.TargetRef)
	}
	if issue, err := checkCheckoutState(ctx, repo.Main, IssueDirtyTarget); err != nil {
		return nil, err
	} else if issue != nil {
		return nil, fmt.Errorf("promote: %s: %v", issue.Code, issue.Detail)
	}
	if issue, err := checkWorktreeState(ctx, plan.WorktreePath, plan.AfterResolution); err != nil {
		return nil, err
	} else if issue != nil {
		return nil, fmt.Errorf("promote: %s: %v", issue.Code, issue.Detail)
	}

	freshTarget, err := headOfContext(ctx, repo.Main)
	if err != nil {
		return nil, err
	}
	freshSource, err := headOfContext(ctx, plan.WorktreePath)
	if err != nil {
		return nil, err
	}
	if freshTarget != plan.TargetOID {
		return &MergeResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "target moved since integration — re-run worktree merge to re-integrate and retry"},
			fmt.Errorf("promote: target moved from %s to %s since integration", plan.TargetOID, freshTarget)
	}
	if freshSource != plan.IntegratedOID {
		return &MergeResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "worktree changed since the verified integration — re-run worktree merge to re-integrate and retry"},
			fmt.Errorf("promote: worktree HEAD moved from the verified integration %s to %s since integration — refusing to promote an unreviewed commit",
				plan.IntegratedOID, freshSource)
	}

	ancestor, err := isAncestor(ctx, repo.Main, freshTarget, freshSource)
	if err != nil {
		return nil, fmt.Errorf("promote: %w", err)
	}
	if !ancestor {
		return &MergeResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "not a fast-forward — re-run worktree merge to re-integrate and retry"},
			fmt.Errorf("promote: %s is not an ancestor of the integrated worktree HEAD %s", freshTarget, freshSource)
	}

	if conflicts, err := SimulateMerge(ctx, repo.Main, freshSource, freshTarget, true); err != nil {
		return nil, fmt.Errorf("promote: re-simulation failed: %w", err)
	} else if len(conflicts) > 0 {
		return nil, fmt.Errorf("promote: re-simulation predicts a conflict in %v — refusing to fall back to a real merge", conflicts)
	}

	if err := ctx.Err(); err != nil {
		return nil, err // never start the ff-only update once canceled — repo.Main stays untouched
	}
	out, err := runGit(ctx, repo.Main, nil, "-c", "submodule.recurse=false", "merge", "--ff-only", "--no-autostash", freshSource)
	if err != nil {
		return nil, fmt.Errorf("git merge --ff-only: %w (%s)", err, firstNonEmpty(gitErrorDetail(err), string(out)))
	}
	return &MergeResult{Phase: "promote", BeforeOID: freshTarget, AfterOID: freshSource}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
