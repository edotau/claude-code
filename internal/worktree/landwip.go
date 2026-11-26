package worktree

import (
	"context"
	"fmt"
)

// commitLaneDirt commits the lane's tracked edits in one commit so the rebase and land carry them; a dry run counts.
func commitLaneDirt(ctx context.Context, repo *Repo, req LandRequest, dryRun bool) error {
	lane, err := resolveWorktree(ctx, repo, req.Name)
	if err != nil {
		return nil // Preflight reports an unknown lane
	}
	gitDir, _, err := gitDirsOf(ctx, lane)
	if err != nil {
		return nil
	}
	if issue, err := inProgressIssue(ctx, lane, gitDir); err != nil || issue != nil {
		return err // a lane mid-operation is left to the plan's in-progress issue
	}
	dirty, err := changedPaths(ctx, lane)
	if err != nil || len(dirty) == 0 {
		return err
	}
	if dryRun {
		fmt.Fprintf(req.Out, "would commit %d uncommitted lane files before landing\n", len(dirty))
		return nil
	}
	msg := "chore: uncommitted work in lane " + req.Name + " at land"
	if out, err := runGit(ctx, lane, nil, "commit", "-a", "-q", "--no-verify", "-m", msg); err != nil {
		return fmt.Errorf("commit the lane's uncommitted files: %w (%s)", err, firstNonEmpty(gitErrorDetail(err), string(out)))
	}
	fmt.Fprintf(req.Out, "committed:  %d uncommitted lane files in %s before landing\n", len(dirty), lane)
	return nil
}
