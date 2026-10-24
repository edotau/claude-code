package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DiscardSummary is what CleanForce is about to throw away, reported before anything is removed.
type DiscardSummary struct {
	Path, Branch, Target string
	Dirty, Unmerged      int
}

// CleanForce removes name's worktree even when dirty or unmerged, then force-deletes its branch.
// It still refuses a worktree that is in use, locked, or the main checkout, and reports the
// DiscardSummary before removing anything; dryRun reports the summary and removes nothing. Runs
// under the per-worktree lease (WithWorktreeLease), like Clean.
func CleanForce(repo *Repo, name string, dryRun bool, report func(DiscardSummary)) (CleanResult, error) {
	var result CleanResult
	err := WithWorktreeLease(repo, name, func() error {
		r, cleanErr := cleanForceInLease(repo, name, dryRun, report)
		result = r
		return cleanErr
	})
	return result, err
}

func cleanForceInLease(repo *Repo, name string, dryRun bool, report func(DiscardSummary)) (CleanResult, error) {
	dest, err := ResolveWorktree(repo, name) // refuses traversal, symlinks, foreign dirs, a root inside Main
	if err != nil {
		return CleanResult{}, err
	}
	if _, err := os.Stat(dest); err != nil {
		return CleanResult{}, fmt.Errorf("worktree %q not found at %s", name, dest)
	}
	branch, _ := currentWorktreeBranch(dest)
	result := CleanResult{Path: dest, Branch: branch}

	if isMainCheckout(repo, dest) {
		result.Reason = "refusing to remove the main checkout"
		return result, nil
	}
	if reason, err := InUseBy(dest); err != nil {
		result.Reason = "in-use check failed: " + err.Error()
		return result, nil
	} else if reason != "" {
		result.Reason = reason
		return result, nil
	}
	locked, err := isLocked(dest)
	if err != nil {
		result.Reason = "lock check failed: " + err.Error()
		return result, nil
	}
	if locked {
		result.Reason = "worktree is locked"
		return result, nil
	}

	ctx := context.Background()
	out, err := gitCmd(ctx, "-C", repo.Main, "symbolic-ref", "--short", "-q", "HEAD").Output()
	target := strings.TrimSpace(string(out))
	if err != nil || target == "" {
		result.Reason = "cannot determine target branch: main checkout is detached or unreadable"
		return result, nil
	}
	status, err := gitCmd(ctx, "-C", dest, "status", "--porcelain").Output()
	if err != nil {
		result.Reason = "status check failed: " + err.Error()
		return result, nil
	}
	count, err := gitCmd(ctx, "-C", dest, "rev-list", "--count", target+"..HEAD").Output()
	if err != nil {
		result.Reason = "unmerged count failed: " + err.Error()
		return result, nil
	}
	unmerged, err := strconv.Atoi(strings.TrimSpace(string(count)))
	if err != nil {
		result.Reason = "unmerged count unreadable: " + err.Error()
		return result, nil
	}
	summary := DiscardSummary{Path: dest, Branch: branch, Target: target, Unmerged: unmerged}
	if s := strings.TrimRight(string(status), "\n"); s != "" {
		summary.Dirty = len(strings.Split(s, "\n"))
	}
	if report != nil {
		report(summary)
	}

	if dryRun {
		result.Removed = true // preview: the remaining refusals passed, so this one would be discarded
		return result, nil
	}
	if out, err := gitCmd(ctx, "-C", repo.Main, "worktree", "remove", "--force", dest).CombinedOutput(); err != nil {
		result.Reason = fmt.Sprintf("git worktree remove --force: %v: %s", err, strings.TrimSpace(string(out)))
		return result, nil
	}
	result.Removed = true
	if branch != "" {
		if out, err := gitCmd(ctx, "-C", repo.Main, "branch", "-D", branch).CombinedOutput(); err != nil {
			result.Reason = fmt.Sprintf("git branch -D %s: %v: %s", branch, err, strings.TrimSpace(string(out)))
		} else {
			result.BranchDeleted = true
		}
	}
	return result, nil
}

// isMainCheckout reports whether dest canonically is repo.Main; an unresolvable path counts as main.
func isMainCheckout(repo *Repo, dest string) bool {
	d, err1 := filepath.EvalSymlinks(dest)
	m, err2 := filepath.EvalSymlinks(repo.Main)
	return err1 != nil || err2 != nil || d == m
}
