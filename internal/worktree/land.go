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
	patch, err := runGit(ctx, main, nil, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--ignore-submodules=none",
		"--src-prefix=a/", "--dst-prefix=b/", "--no-relative", "--no-color", "--submodule=short", from, to, "--")
	if err != nil {
		return fmt.Errorf("git diff %s..%s: %w", ShortOID(from), ShortOID(to), err)
	}
	if len(patch) == 0 {
		return nil
	}
	if gitlinkPatch.Match(patch) {
		return fmt.Errorf("the lane changes a submodule, which cannot land uncommitted; use --merge: %w", ErrLandRefused)
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
	return strings.FieldsFunc(string(out), func(r rune) bool { return r == '\x00' }), nil
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
	printLanded(ctx, repo, req, landed, "branch "+branch+" kept")
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
