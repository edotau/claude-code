package worktree

import (
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

// Merge runs Preflight, refuses outright on any blocking Issue (including a predicted conflict —
// Phase 3 resolves those), then integrates and promotes. On a failure after IntegrateTarget it still
// returns the plan and the partial result, so the caller can inspect state instead of guessing it.
func Merge(repo *Repo, name, target string) (*MergePlan, *MergeResult, error) {
	plan, err := Preflight(repo, name, target)
	if err != nil {
		return nil, nil, err
	}
	if len(plan.Issues) > 0 {
		return plan, nil, fmt.Errorf("worktree merge blocked: %s", issueSummary(plan.Issues))
	}

	integrated, err := IntegrateTarget(repo, plan)
	if err != nil {
		return plan, nil, err
	}
	promoted, err := Promote(repo, plan)
	if err != nil {
		return plan, integrated, err
	}
	return plan, promoted, nil
}

// IntegrateTarget merges the pinned target OID into the worktree itself — never repo.Main — so any
// integration commit lands in the worktree's own history. Callers gate on plan.Issues (a predicted
// conflict) before calling this; Phase 3 adds resolution for the ones that stop here instead.
func IntegrateTarget(repo *Repo, plan *MergePlan) (*MergeResult, error) {
	if issue, err := checkCheckoutState(plan.WorktreePath, IssueDirtyWorktree); err != nil {
		return nil, err
	} else if issue != nil {
		return nil, fmt.Errorf("integrate: %s: %v", issue.Code, issue.Detail)
	}
	before := headOf(plan.WorktreePath)
	out, err := runGit(plan.WorktreePath, nil, "merge", "--no-edit", plan.TargetOID)
	if err != nil {
		return nil, fmt.Errorf("git merge inside worktree: %w (%s)", err, firstNonEmpty(gitErrorDetail(err), string(out)))
	}
	after := headOf(plan.WorktreePath)
	return &MergeResult{Phase: "integrate", BeforeOID: before, AfterOID: after}, nil
}

// Promote reacquires the merge lock, rechecks both OIDs and checkout states against what Preflight
// pinned, requires ancestry, and fast-forwards repo.Main onto the integrated worktree HEAD — never a
// real merge commit. Target movement or a failed ancestry/re-simulation check stops with a retry hint;
// this never falls back to a real conflicting merge.
func Promote(repo *Repo, plan *MergePlan) (*MergeResult, error) {
	var result *MergeResult
	err := Acquire(0, func() error {
		r, lockedErr := promoteLocked(repo, plan)
		result = r
		return lockedErr
	})
	return result, err
}

func promoteLocked(repo *Repo, plan *MergePlan) (*MergeResult, error) {
	branch, err := requireBranch(repo.Main)
	if err != nil {
		return nil, fmt.Errorf("promote: %w", err)
	}
	if branch != plan.TargetRef {
		return nil, fmt.Errorf("promote: main checkout moved to %q, pinned target was %q — retry from a fresh plan", branch, plan.TargetRef)
	}
	if issue, err := checkCheckoutState(repo.Main, IssueDirtyTarget); err != nil {
		return nil, err
	} else if issue != nil {
		return nil, fmt.Errorf("promote: %s: %v", issue.Code, issue.Detail)
	}
	if issue, err := checkCheckoutState(plan.WorktreePath, IssueDirtyWorktree); err != nil {
		return nil, err
	} else if issue != nil {
		return nil, fmt.Errorf("promote: %s: %v", issue.Code, issue.Detail)
	}

	freshTarget := headOf(repo.Main)
	freshSource := headOf(plan.WorktreePath)
	if freshTarget != plan.TargetOID {
		return &MergeResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "target moved since integration — re-run worktree merge to re-integrate and retry"},
			fmt.Errorf("promote: target moved from %s to %s since integration", plan.TargetOID, freshTarget)
	}

	ancestor, err := isAncestor(repo.Main, freshTarget, freshSource)
	if err != nil {
		return nil, fmt.Errorf("promote: %w", err)
	}
	if !ancestor {
		return &MergeResult{Phase: "promote", BeforeOID: freshTarget, ResumeHint: "not a fast-forward — re-run worktree merge to re-integrate and retry"},
			fmt.Errorf("promote: %s is not an ancestor of the integrated worktree HEAD %s", freshTarget, freshSource)
	}

	if conflicts, err := SimulateMerge(repo.Main, freshSource, freshTarget, true); err != nil {
		return nil, fmt.Errorf("promote: re-simulation failed: %w", err)
	} else if len(conflicts) > 0 {
		return nil, fmt.Errorf("promote: re-simulation predicts a conflict in %v — refusing to fall back to a real merge", conflicts)
	}

	out, err := runGit(repo.Main, nil, "-c", "submodule.recurse=false", "merge", "--ff-only", "--no-autostash", freshSource)
	if err != nil {
		return nil, fmt.Errorf("git merge --ff-only: %w (%s)", err, firstNonEmpty(gitErrorDetail(err), string(out)))
	}
	return &MergeResult{Phase: "promote", BeforeOID: freshTarget, AfterOID: freshSource}, nil
}

func issueSummary(issues []Issue) string {
	codes := make([]string, len(issues))
	for i, iss := range issues {
		codes[i] = iss.Code
	}
	return strings.Join(codes, ", ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
