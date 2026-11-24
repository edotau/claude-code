package worktree

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/review/bench"
)

// ErrLandRefused marks a land stopped before it changed the live tree: a landed path is dirty or the apply failed.
var ErrLandRefused = errors.New("land refused")

const gitlinkMode = "160000"

// gitlinkPatch matches a patch header touching a submodule entry, which git apply cannot update without an index.
var gitlinkPatch = regexp.MustCompile(`(?m)^(?:index \S+ ` + gitlinkMode + `|(?:old|new|deleted file|new file) mode ` + gitlinkMode + `)`)

// applyLaneDiff applies the net diff from..to unstaged: --check first, then an apply a cancel cannot cut short.
func applyLaneDiff(ctx context.Context, main, from, to string) error {
	patch, err := runGit(ctx, main, nil, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none", from, to, "--")
	if err != nil {
		return fmt.Errorf("git diff %s..%s: %w", ShortOID(from), ShortOID(to), err)
	}
	if len(patch) == 0 {
		return nil
	}
	if gitlinkPatch.Match(patch) {
		return fmt.Errorf("the lane changes a submodule, which cannot land uncommitted; use --merge: %w", ErrLandRefused)
	}
	check := gitCmd(ctx, "-C", main, "apply", "--check", "--binary", "-")
	check.Stdin = bytes.NewReader(patch)
	if out, err := check.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply --check in %s: %s; nothing changed: %w", main, strings.TrimSpace(string(out)), ErrLandRefused)
	}
	apply := gitCmd(context.WithoutCancel(ctx), "-C", main, "apply", "--binary", "-")
	apply.Stdin = bytes.NewReader(patch)
	if out, err := apply.CombinedOutput(); err != nil {
		return fmt.Errorf("git apply in %s failed after a clean --check, the live tree may be partly changed: %s: %w",
			main, strings.TrimSpace(string(out)), err)
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
	base, err := runGit(ctx, repo.Main, nil, "merge-base", "HEAD", branch)
	if err != nil {
		result.Reason = "no common history with HEAD: " + err.Error()
		return result, nil
	}
	names, err := runGit(ctx, repo.Main, nil, "diff", "--name-only", "-z", "--no-renames", "--ignore-submodules=none",
		strings.TrimSpace(string(base)), branch, "--")
	if err != nil {
		return result, fmt.Errorf("git diff %s: %w", branch, err)
	}
	files := strings.FieldsFunc(string(names), func(r rune) bool { return r == '\x00' })
	args := append([]string{"diff", "--quiet", "--ignore-submodules=none", "HEAD", branch, "--"}, files...)
	if _, err := runGit(ctx, repo.Main, nil, args...); err != nil {
		if code, isExit := asExitErr(err); !isExit || code != 1 {
			return result, fmt.Errorf("git diff HEAD %s: %w", branch, err)
		}
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
