package worktree

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Bounded-wait knobs and the one seam over the real agent dispatch; a test shrinks the first two and
// fakes the clock/agent through the vars so the whole poll loop runs in milliseconds, never a real model.
var (
	nowFn          = time.Now
	sleepFn        = time.Sleep
	pollInterval   = 2 * time.Second
	maxResolveWait = 10 * time.Minute
)

// resolveAgentResult is the subset of `ask --format json`'s envelope ResolveWithAgent inspects.
type resolveAgentResult struct {
	Fallback string
}

// askEnvelope mirrors the JSON `claude-code ask --format json` prints: agent.Result's fields plus the
// top-level "error". Decoded here rather than importing internal/agent, which would cycle back through
// internal/launch -> internal/hooks -> internal/worktree.
type askEnvelope struct {
	Answer         string `json:"answer"`
	Fallback       string `json:"fallback,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
	Error          string `json:"error,omitempty"`
}

// runResolveAgent shells out to this same claude-code binary's own `ask` verb — --fallback none so an
// explicitly selected codex leg either runs as codex or fails outright, never silently as claude — once,
// with tools enabled, on prompt inside workDir. It never retries.
var runResolveAgent = func(agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
	bin, err := claudeCodeBin()
	if err != nil {
		return resolveAgentResult{}, fmt.Errorf("resolve: locate claude-code binary: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+30*time.Second) // belt-and-suspenders over ask's own --timeout
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "ask", "--agent", agentName, "--workdir", workDir,
		"--tools", "--fallback", "none", "--timeout", timeout.String(), "--format", "json", "-")
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, runErr := cmd.Output()

	var env askEnvelope
	if jsonErr := json.Unmarshal(stdout, &env); jsonErr != nil {
		if runErr != nil {
			return resolveAgentResult{}, fmt.Errorf("claude-code ask: %w (%s)", runErr, strings.TrimSpace(stderr.String()))
		}
		return resolveAgentResult{}, fmt.Errorf("claude-code ask: unparseable output: %w", jsonErr)
	}
	res := resolveAgentResult{Fallback: env.Fallback}
	if env.Error != "" {
		return res, fmt.Errorf("claude-code ask: %s", env.Error)
	}
	if runErr != nil {
		return res, fmt.Errorf("claude-code ask: %w (%s)", runErr, strings.TrimSpace(stderr.String()))
	}
	return res, nil
}

// claudeCodeBin resolves the binary to self-exec: this code already runs inside it, so os.Executable
// is the common case; PATH lookup is the fallback for an unusual invocation (e.g. `go run`).
func claudeCodeBin() (string, error) {
	if exe, err := os.Executable(); err == nil {
		return exe, nil
	}
	return exec.LookPath("claude-code")
}

// resolutionState is CheckResolution's three-way verdict: still mid-resolution, a verified clean
// commit, or a state no further waiting will fix (an abort, or a stale merge commit).
type resolutionState int

const (
	resolutionPending resolutionState = iota
	resolutionResolved
	resolutionInvalid
)

// ResolveWithAgent drives one agent-mediated conflict resolution to completion. op == nil starts
// fresh: it materializes plan's merge-tree-predicted conflict for real inside the worktree (never
// repo.Main) and invokes agentName once, with tools enabled, to resolve and commit it there. op != nil
// is the --resume path: it skips straight to polling the saved operation — a tools-enabled agent is
// never re-invoked automatically, crash or not. Either way, only Promote (the caller's job) commits
// repo.Main; this function only polls and reports.
func ResolveWithAgent(repo *Repo, plan *MergePlan, agentName string, op *Operation) (*Operation, *MergeResult, error) {
	if op == nil {
		materialized, err := materializeConflict(plan)
		if err != nil {
			return nil, nil, err
		}
		if materialized.Phase != "conflict" {
			return nil, materialized, nil // merge-tree's prediction didn't reproduce; nothing to hand the agent
		}

		op = &Operation{
			ID:                newOperationID(),
			Repo:              repo.Main,
			Worktree:          filepath.Base(plan.WorktreePath),
			TargetOID:         plan.TargetOID,
			OriginalSourceOID: plan.SourceOID,
			Phase:             "resolving",
			StartedAt:         nowFn(),
			Agent:             agentName,
		}
		op.Deadline = op.StartedAt.Add(maxResolveWait)
		if err := SaveOperation(op); err != nil {
			return nil, nil, err
		}

		res, runErr := runResolveAgent(agentName, resolvePrompt(plan), plan.WorktreePath, maxResolveWait)
		if runErr != nil {
			return failOperation(op, fmt.Errorf("resolve: agent %s failed: %w", agentName, runErr))
		}
		if res.Fallback != "" {
			return failOperation(op, fmt.Errorf(
				"resolve: requested agent %q fell back to %q — refusing a silently different model", agentName, res.Fallback))
		}
	}
	return pollUntilResolved(op, plan.WorktreePath)
}

// materializeConflict turns plan's merge-tree-predicted conflict into a real one inside the worktree
// via an actual `git merge` — conflict markers plus MERGE_HEAD, ready for an agent to resolve and
// commit there. A merge that unexpectedly completes cleanly is reported as Phase "integrate" instead,
// so the caller knows there's nothing left for an agent to do.
func materializeConflict(plan *MergePlan) (*MergeResult, error) {
	if issue, err := checkCheckoutState(plan.WorktreePath, IssueDirtyWorktree); err != nil {
		return nil, err
	} else if issue != nil {
		return nil, fmt.Errorf("resolve: %s: %v", issue.Code, issue.Detail)
	}
	before := headOf(plan.WorktreePath)
	out, mergeErr := runGit(plan.WorktreePath, nil, "merge", "--no-edit", plan.TargetOID)
	if mergeErr == nil {
		return &MergeResult{Phase: "integrate", BeforeOID: before, AfterOID: headOf(plan.WorktreePath)}, nil
	}
	mergeHead, err := hasMergeHead(plan.WorktreePath)
	if err != nil {
		return nil, err
	}
	if !mergeHead {
		return nil, fmt.Errorf("git merge inside worktree: %w (%s)", mergeErr, firstNonEmpty(gitErrorDetail(mergeErr), string(out)))
	}
	return &MergeResult{Phase: "conflict", BeforeOID: before}, nil
}

// pollUntilResolved is the one loop both a fresh ResolveWithAgent call and --resume share: it checks
// CheckResolution every pollInterval until resolved, invalid, or op.Deadline passes, updating and
// re-saving op at every terminal transition so the operation file always reflects the last known state.
func pollUntilResolved(op *Operation, worktreePath string) (*Operation, *MergeResult, error) {
	for {
		state, reason, err := CheckResolution(op, worktreePath)
		if err != nil {
			return failOperation(op, err)
		}
		switch state {
		case resolutionResolved:
			op.Phase = "resolved"
			_ = SaveOperation(op)
			return op, &MergeResult{Phase: "integrate", BeforeOID: op.OriginalSourceOID, AfterOID: headOf(worktreePath)}, nil
		case resolutionInvalid:
			return failOperation(op, fmt.Errorf("resolve: %s", reason))
		}
		if !nowFn().Before(op.Deadline) {
			op.Phase = "timeout"
			_ = SaveOperation(op)
			return op, nil, fmt.Errorf("resolve: no clean, committed resolution within %s of starting", maxResolveWait)
		}
		sleepFn(pollInterval)
	}
}

// CheckResolution inspects worktreePath's live git state against op's pinned OIDs — the one seam both
// the poll loop and a crash-and-resume use, so resuming revalidates exactly what a live poll would
// have. MERGE_HEAD still set, or any unmerged path left, means the agent is still mid-resolution.
// Uncommitted changes once MERGE_HEAD clears, or a HEAD unchanged since the agent started, means the
// merge was aborted rather than resolved. Both pinned commits must be ancestors of the new HEAD,
// rejecting a stale merge commit left over from an earlier aborted attempt.
func CheckResolution(op *Operation, worktreePath string) (resolutionState, string, error) {
	unmerged, err := unmergedPaths(worktreePath)
	if err != nil {
		return resolutionPending, "", err
	}
	mergeHead, err := hasMergeHead(worktreePath)
	if err != nil {
		return resolutionPending, "", err
	}
	if mergeHead || len(unmerged) > 0 {
		return resolutionPending, "", nil
	}

	dirty, err := isDirty(worktreePath)
	if err != nil {
		return resolutionPending, "", err
	}
	if dirty {
		return resolutionInvalid, "worktree has uncommitted changes outside a merge, not a committed resolution", nil
	}

	head := headOf(worktreePath)
	if head == "" {
		return resolutionPending, "", fmt.Errorf("resolve: could not resolve HEAD in %s", worktreePath)
	}
	if head == op.OriginalSourceOID {
		return resolutionInvalid, "HEAD is unchanged since the agent started — the merge was aborted, not resolved", nil
	}

	for _, pinned := range []string{op.TargetOID, op.OriginalSourceOID} {
		ok, err := isAncestor(worktreePath, pinned, head)
		if err != nil {
			return resolutionPending, "", err
		}
		if !ok {
			return resolutionInvalid, fmt.Sprintf(
				"pinned commit %s is not an ancestor of HEAD %s — rejecting a stale or unrelated merge commit", pinned, head), nil
		}
	}
	return resolutionResolved, "", nil
}

// unmergedPaths reads `git diff --name-only --diff-filter=U -z`: every path still carrying an unmerged
// (conflicted) index stage. Empty means every conflict the agent was handed has been staged/resolved.
func unmergedPaths(path string) ([]string, error) {
	out, err := runGit(path, nil, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, fmt.Errorf("git diff --diff-filter=U in %s: %w", path, err)
	}
	var paths []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			paths = append(paths, f)
		}
	}
	return paths, nil
}

// hasMergeHead reports whether path's own git-dir still has an in-progress merge recorded.
func hasMergeHead(path string) (bool, error) {
	_, err := runGit(path, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD")
	if err == nil {
		return true, nil
	}
	if code, ok := asExitErr(err); ok && code == 1 {
		return false, nil
	}
	return false, err
}

// failOperation marks op failed, best-effort re-saves it (the operation file is preserved either way,
// per the "never auto-retry" contract — resume decides what to do next, this function never cleans up),
// and returns it alongside err.
func failOperation(op *Operation, err error) (*Operation, *MergeResult, error) {
	op.Phase = "failed"
	_ = SaveOperation(op)
	return op, nil, err
}

// resolvePrompt frames the task for the agent: a resolved state is a clean, committed merge in that
// worktree — never a reply describing one, and never a touch to any other checkout.
func resolvePrompt(plan *MergePlan) string {
	return fmt.Sprintf(
		"The git worktree at %s has a real merge conflict (MERGE_HEAD is set there), merging branch %q "+
			"(target commit %s) into it. Resolve every conflicted file in that worktree only, then run "+
			"`git add` on each resolved path and `git commit --no-edit` to finish the in-progress merge. "+
			"Do not run `git merge --abort`. Do not touch any other git checkout. When you are done, HEAD "+
			"in that worktree must be a new commit with both %s and %s as ancestors.",
		plan.WorktreePath, plan.TargetRef, plan.TargetOID, plan.TargetOID, plan.SourceOID)
}

// newOperationID is a short random tag for one saved Operation; collisions are harmless since the file
// path is keyed by repo+worktree, not by this id.
func newOperationID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("op-%d", nowFn().UnixNano())
	}
	return "op-" + hex.EncodeToString(b)
}
