package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/review/bench"
)

// ErrLandRefused marks a land stopped before it changed the live tree: a landed path is dirty or the apply failed.
var ErrLandRefused = errors.New("land refused")

// applyLaneDiff applies the net diff from..to to main's working tree, unstaged; all or nothing.
func applyLaneDiff(ctx context.Context, main, from, to string) error {
	patch, err := runGit(ctx, main, nil, "diff", "--binary", "--no-ext-diff", "--no-textconv", from, to, "--")
	if err != nil {
		return fmt.Errorf("git diff %s..%s: %w", ShortOID(from), ShortOID(to), err)
	}
	if len(patch) == 0 {
		return nil
	}
	cmd := gitCmd(ctx, "-C", main, "apply", "--binary", "-")
	cmd.Stdin = bytes.NewReader(patch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply in %s changed nothing: %s: %w", main, strings.TrimSpace(string(out)), ErrLandRefused)
	}
	return nil
}

// afterLand reports the land, then removes the lane's worktree (its branch stays until clean) and starts the audit.
func afterLand(ctx context.Context, repo *Repo, plan *MergePlan, req MergeRequest, landed *MergeResult) (*mergeOutcome, error) {
	if req.AfterLand != nil {
		req.AfterLand(landed.AfterOID)
	}
	if err := bench.ApplyPending(repo.Main, req.Name); err != nil {
		fmt.Fprintf(req.Err, "worktree land: warning: bench pending: %v\n", err)
	}
	if err := Dequeue(plan.WorktreePath); err != nil {
		fmt.Fprintf(req.Err, "worktree land: warning: merge queue: %v\n", err)
	}
	// the lane's rebased head is exactly live HEAD plus the landed diff
	if line := StartAudit(repo.Main, landed.AfterOID); line != "" {
		fmt.Fprintln(req.Out, line)
	}
	branch, _ := currentWorktreeBranch(plan.WorktreePath)
	out := &mergeOutcome{}
	closing := LedgerClose{Via: "land", MergeSHA: landed.AfterOID, Head: landed.AfterOID}
	if !req.NoClean {
		result, err := removeLanded(repo, req.Name, plan.WorktreePath, req.Err, closing)
		out.Clean = &result
		if err != nil {
			return nil, fmt.Errorf("clean: %w", err)
		}
	}
	state := "branch " + branch + " kept"
	if out.Clean != nil && out.Clean.BranchDeleted {
		state = "branch " + branch + " deleted"
	}
	printLanded(ctx, repo, req, landed, state)
	return out, nil
}

// printLanded prints the one-line result, the landed diff stat and where plan ticks went.
func printLanded(ctx context.Context, repo *Repo, req MergeRequest, landed *MergeResult, state string) {
	files, _ := targetTouchedPaths(ctx, repo.Main, landed.BeforeOID+".."+landed.AfterOID)
	fmt.Fprintf(req.Out, "landed %s: %d files uncommitted in %s; %s\n", req.Name, len(files), repo.Main, state)
	if stat, err := runGit(ctx, repo.Main, nil, "diff", "--stat", landed.BeforeOID, landed.AfterOID, "--"); err == nil {
		fmt.Fprint(req.Out, string(stat))
	}
	fmt.Fprintln(req.Out, "plan ticks: none at landing; they tick when you commit with Task: trailers or run `plan task --done`")
}

// removeLanded removes a landed lane's worktree once it is idle; its branch stays unless it is already merged.
func removeLanded(repo *Repo, name, dest string, log io.Writer, closing LedgerClose) (CleanResult, error) {
	result, err := beginClean(name, dest, log)
	if err != nil {
		return CleanResult{}, err
	}
	if _, reason := cleanBusyReason(dest, "", nil); reason != "" {
		result.Reason = reason
		return result, nil
	}
	return removeMerged(repo, name, dest, Entry{Head: headOf(dest)}, result, log, closing), nil
}

// cleanLandedBranch deletes name's kept branch once its landed diff is in live HEAD; there is no worktree left.
func cleanLandedBranch(repo *Repo, name string, dryRun bool) (CleanResult, error) {
	branch := "worktree-" + name
	if !branchExists(repo.Main, branch) {
		return CleanResult{}, fmt.Errorf("worktree %q not found, and no kept branch %s", name, branch)
	}
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	result := CleanResult{Path: branch, Branch: branch}
	base, err := runGit(ctx, repo.Main, nil, "merge-base", "HEAD", branch)
	if err != nil {
		result.Reason = "no common history with HEAD: " + err.Error()
		return result, nil
	}
	files, err := targetTouchedPaths(ctx, repo.Main, strings.TrimSpace(string(base))+".."+branch)
	if err != nil {
		return result, err
	}
	if _, err := runGit(ctx, repo.Main, nil, append([]string{"diff", "--quiet", "HEAD", branch, "--"}, files...)...); err != nil {
		result.Reason = "branch " + branch + " has changes not in HEAD yet: commit the landed files first"
		return result, nil
	}
	result.Removed = true
	if dryRun {
		return result, nil
	}
	if out, err := runGit(ctx, repo.Main, nil, "branch", "-D", branch); err != nil {
		result.Removed, result.Reason = false, strings.TrimSpace(string(out))
		return result, nil
	}
	result.BranchDeleted = true
	return result, nil
}
