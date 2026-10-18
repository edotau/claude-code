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

	"github.com/edotau/claude-code/internal/paths"
)

// leasedFileLock tries once (no wait) for the exclusive lease at path; ran=false means another
// resolve/resume/merge/clean already holds it. Unlike paths.WithFileLock, TryFileLock does not create
// path's parent directory, so this does it first — the operations dir may not exist yet on a first resolve.
func leasedFileLock(path string, fn func() error) (ran bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return paths.TryFileLock(path, 0, fn)
}

// WithWorktreeLease runs fn holding the exclusive per-worktree operation lease for repo+name: the one
// invariant serializing a resolve, an ordinary merge, and a Clean on the same worktree so they can
// never interleave. Always acquired before Promote's own merge lock, never the reverse — Merge and the
// CLI's merge lifecycle both nest Promote inside this lease, so that is the only nesting order that
// exists in this codebase and the two locks cannot deadlock against each other.
func WithWorktreeLease(repo *Repo, name string, fn func() error) error {
	ran, err := leasedFileLock(operationLeasePath(repo.Main, name), fn)
	if err != nil {
		return err
	}
	if !ran {
		return fmt.Errorf("worktree %q: another merge, resolve, or clean operation is already active", name)
	}
	return nil
}

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
// with tools enabled, on prompt inside workDir. It never retries. The agent only edits files; it is
// told not to run git add/commit/merge (a sandboxed agent can't write inside .git anyway) — the
// controller (finalizeAgentResolution) stages and commits its edits once it exits.
var runResolveAgent = func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (resolveAgentResult, error) {
	bin, err := claudeCodeBin()
	if err != nil {
		return resolveAgentResult{}, fmt.Errorf("resolve: locate claude-code binary: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second) // belt-and-suspenders over ask's own --timeout
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, "ask", "--agent", agentName, "--workdir", workDir,
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

// ResolveWithAgent drives one agent-mediated conflict resolution to completion. The caller must already
// hold WithWorktreeLease(repo, name, ...) for the full span of this call — this function does no
// locking of its own. op == nil starts fresh: it materializes plan's merge-tree-predicted conflict for
// real inside the worktree (never repo.Main), invokes agentName once with tools enabled to edit (not
// commit) the conflicted files, then the controller itself verifies and commits — see
// finalizeAgentResolution. op != nil is the --resume path: it requires a persisted, successful,
// fallback-free agent-exit receipt (op.AgentExitOK) before even polling — a failed, timed-out, or still
// running operation can never be waved through by git state alone — then skips straight to polling the
// saved operation (for a human or an agent that committed on its own despite the instruction not to); a
// tools-enabled agent is never re-invoked automatically, crash or not. Either way, only Promote (the
// caller's job) commits repo.Main; this function only edits/polls the worktree and reports.
func ResolveWithAgent(ctx context.Context, repo *Repo, plan *MergePlan, agentName string, op *Operation) (*Operation, *MergeResult, error) {
	resuming := op != nil
	if resuming && !op.AgentExitOK {
		return nil, nil, fmt.Errorf(
			"resolve: operation %s has no persisted successful agent-exit receipt (phase %q) — the agent may still be running or never completed; refusing to resume", op.ID, op.Phase)
	}
	return resolveWithAgent(ctx, repo, plan, agentName, op)
}

func resolveWithAgent(ctx context.Context, repo *Repo, plan *MergePlan, agentName string, op *Operation) (*Operation, *MergeResult, error) {
	if op == nil {
		materialized, err := materializeConflict(ctx, plan)
		if err != nil {
			return nil, nil, err
		}
		if materialized.Phase != "conflict" {
			return nil, materialized, nil // merge-tree's prediction didn't reproduce; nothing to hand the agent
		}
		conflictPaths, err := unmergedPaths(ctx, plan.WorktreePath)
		if err != nil {
			return nil, nil, err
		}

		op = &Operation{
			ID:                newOperationID(),
			Repo:              repo.Main,
			Worktree:          filepath.Base(plan.WorktreePath),
			TargetRef:         plan.TargetRef,
			TargetOID:         plan.TargetOID,
			OriginalSourceOID: plan.SourceOID,
			ConflictPaths:     conflictPaths,
			Phase:             "resolving",
			StartedAt:         nowFn(),
			Agent:             agentName,
		}
		op.Deadline = op.StartedAt.Add(maxResolveWait)
		if err := SaveOperation(op); err != nil {
			return nil, nil, err
		}

		res, runErr := runResolveAgent(ctx, agentName, resolvePrompt(plan, conflictPaths), plan.WorktreePath, maxResolveWait)
		if runErr != nil {
			return failOperation(op, fmt.Errorf("resolve: agent %s failed: %w", agentName, runErr))
		}
		if res.Fallback != "" {
			return failOperation(op, fmt.Errorf(
				"resolve: requested agent %q fell back to %q — refusing a silently different model", agentName, res.Fallback))
		}
		op.AgentExitOK = true // the one receipt resume is allowed to trust instead of re-running the agent
		if err := SaveOperation(op); err != nil {
			return nil, nil, err
		}
		return finalizeAgentResolution(ctx, op, plan.WorktreePath)
	}
	return pollUntilResolved(ctx, op, plan.WorktreePath)
}

// materializeConflict turns plan's merge-tree-predicted conflict into a real one inside the worktree
// via an actual `git merge` — conflict markers plus MERGE_HEAD, ready for an agent to resolve. A merge
// that unexpectedly completes cleanly is reported as Phase "integrate" instead, so the caller knows
// there's nothing left for an agent to do.
func materializeConflict(ctx context.Context, plan *MergePlan) (*MergeResult, error) {
	if issue, err := checkCheckoutState(ctx, plan.WorktreePath, IssueDirtyWorktree); err != nil {
		return nil, err
	} else if issue != nil {
		return nil, fmt.Errorf("resolve: %s: %v", issue.Code, issue.Detail)
	}
	before := headOf(plan.WorktreePath)
	out, mergeErr := runGit(ctx, plan.WorktreePath, nil, "merge", "--no-edit", plan.TargetOID)
	if mergeErr == nil {
		return &MergeResult{Phase: "integrate", BeforeOID: before, AfterOID: headOf(plan.WorktreePath)}, nil
	}
	mergeHead, err := hasMergeHead(ctx, plan.WorktreePath)
	if err != nil {
		return nil, err
	}
	if !mergeHead {
		return nil, fmt.Errorf("git merge inside worktree: %w (%s)", mergeErr, firstNonEmpty(gitErrorDetail(mergeErr), string(out)))
	}
	return &MergeResult{Phase: "conflict", BeforeOID: before}, nil
}

// conflictMarkerPrefixes are the three literal marker lines a leftover, unresolved conflict always
// carries — scanned directly in addition to `git diff --check`'s own leftover-marker detection, so a
// corner case in one check is still caught by the other.
var conflictMarkerPrefixes = []string{"<<<<<<<", "=======", ">>>>>>>"}

// conflictMarkersClear reports whether every path in paths is free of leftover conflict markers, via
// `git diff --check` (git's own leftover-marker detector) and a direct content scan. badPath names one
// offending path when clean is false; err means the check itself could not be trusted.
func conflictMarkersClear(ctx context.Context, worktreePath string, paths []string) (clean bool, badPath string, err error) {
	if len(paths) == 0 {
		return true, "", nil
	}
	args := append([]string{"diff", "--check", "--"}, paths...)
	if out, checkErr := runGit(ctx, worktreePath, nil, args...); checkErr != nil {
		if _, isExit := asExitErr(checkErr); isExit {
			return false, strings.TrimSpace(string(out)), nil // git diff --check found a leftover marker
		}
		return false, "", fmt.Errorf("git diff --check in %s: %w", worktreePath, checkErr)
	}
	for _, p := range paths {
		data, readErr := os.ReadFile(filepath.Join(worktreePath, p))
		if readErr != nil {
			continue // tracked as resolved but no longer on disk (e.g. deleted) — nothing to scan
		}
		for _, line := range strings.Split(string(data), "\n") {
			for _, prefix := range conflictMarkerPrefixes {
				if strings.HasPrefix(line, prefix) {
					return false, p, nil
				}
			}
		}
	}
	return true, "", nil
}

// changedPaths reads every path `git status --porcelain` reports as touched (staged or not), tracked
// or untracked — the universe finalizeAgentResolution checks against op.ConflictPaths.
// changedPaths lists TRACKED changes only: untracked files an agent's own tooling drops (plugin caches) are never
// staged, and Clean still refuses the worktree while they exist, so they cannot ride into the commit or be lost.
func changedPaths(ctx context.Context, worktreePath string) ([]string, error) {
	out, err := runGit(ctx, worktreePath, nil, "status", "--porcelain", "--ignore-submodules=none", "--untracked-files=no")
	if err != nil {
		return nil, fmt.Errorf("git status in %s: %w", worktreePath, err)
	}
	var result []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		result = append(result, strings.TrimSpace(line[3:]))
	}
	return result, nil
}

// outsideConflictPaths reports every entry of changed not present in conflictPaths.
func outsideConflictPaths(changed, conflictPaths []string) []string {
	allowed := make(map[string]bool, len(conflictPaths))
	for _, p := range conflictPaths {
		allowed[p] = true
	}
	var extra []string
	for _, p := range changed {
		if !allowed[p] {
			extra = append(extra, p)
		}
	}
	return extra
}

// finalizeAgentResolution is the controller's half of the new contract: a sandboxed agent (e.g. codex's
// workspace-write) can edit files but can't write inside .git, so it is told to edit only — this
// function verifies every originally-conflicted path lost its markers and that nothing outside those
// paths changed, then runs `git add` + `git commit --no-edit` itself. Either check failing fails the
// operation outright (like a timeout: the operation file and the worktree's edits are preserved,
// nothing here falls back to polling) since no further agent turn is coming.
func finalizeAgentResolution(ctx context.Context, op *Operation, worktreePath string) (*Operation, *MergeResult, error) {
	clean, badPath, err := conflictMarkersClear(ctx, worktreePath, op.ConflictPaths)
	if err != nil {
		return failOperation(op, err)
	}
	if !clean {
		return failOperation(op, fmt.Errorf("resolve: conflict markers remain in %q after the agent exited — refusing to stage or commit", badPath))
	}
	changed, err := changedPaths(ctx, worktreePath)
	if err != nil {
		return failOperation(op, err)
	}
	if extra := outsideConflictPaths(changed, op.ConflictPaths); len(extra) > 0 {
		return failOperation(op, fmt.Errorf("resolve: agent changed %v outside the conflicted paths %v — refusing to stage or commit", extra, op.ConflictPaths))
	}

	addArgs := append([]string{"add", "--"}, op.ConflictPaths...)
	if out, err := runGit(ctx, worktreePath, nil, addArgs...); err != nil {
		return failOperation(op, fmt.Errorf("git add in %s: %w (%s)", worktreePath, err, firstNonEmpty(gitErrorDetail(err), string(out))))
	}
	if out, err := runGit(ctx, worktreePath, nil, "commit", "--no-edit"); err != nil {
		return failOperation(op, fmt.Errorf("git commit in %s: %w (%s)", worktreePath, err, firstNonEmpty(gitErrorDetail(err), string(out))))
	}

	state, reason, err := CheckResolution(ctx, op, worktreePath)
	if err != nil {
		return failOperation(op, err)
	}
	if state != resolutionResolved {
		return failOperation(op, fmt.Errorf("resolve: %s", firstNonEmpty(reason, "commit did not produce a resolved state")))
	}
	op.Phase = "resolved"
	_ = SaveOperation(op)
	return op, &MergeResult{Phase: "integrate", BeforeOID: op.OriginalSourceOID, AfterOID: headOf(worktreePath)}, nil
}

// pollUntilResolved is the fallback path for --resume: nothing re-invokes the agent, so this only
// detects a human (or an agent that committed on its own despite the instruction not to) finishing the
// job out of band. It checks CheckResolution every pollInterval until resolved, invalid, canceled, or
// op.Deadline passes, updating and re-saving op at every terminal transition.
func pollUntilResolved(ctx context.Context, op *Operation, worktreePath string) (*Operation, *MergeResult, error) {
	for {
		if err := ctx.Err(); err != nil {
			op.Phase = "cancelled"
			_ = SaveOperation(op)
			return op, nil, fmt.Errorf("resolve: cancelled: %w", err)
		}
		state, reason, err := CheckResolution(ctx, op, worktreePath)
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
// the poll loop and finalizeAgentResolution's post-commit check use, so resuming or finalizing
// revalidates exactly what a live poll would have. MERGE_HEAD still set, or any unmerged path left,
// means the agent is still mid-resolution. Uncommitted changes once MERGE_HEAD clears, or a HEAD
// unchanged since the agent started, means the merge was aborted rather than resolved. Both pinned
// commits must be ancestors of the new HEAD, rejecting a stale merge commit left over from an earlier
// aborted attempt.
func CheckResolution(ctx context.Context, op *Operation, worktreePath string) (resolutionState, string, error) {
	unmerged, err := unmergedPaths(ctx, worktreePath)
	if err != nil {
		return resolutionPending, "", err
	}
	mergeHead, err := hasMergeHead(ctx, worktreePath)
	if err != nil {
		return resolutionPending, "", err
	}
	if mergeHead || len(unmerged) > 0 {
		return resolutionPending, "", nil
	}

	tracked, err := changedPaths(ctx, worktreePath)
	if err != nil {
		return resolutionPending, "", err
	}
	if len(tracked) > 0 {
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
		ok, err := isAncestor(ctx, worktreePath, pinned, head)
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
func unmergedPaths(ctx context.Context, path string) ([]string, error) {
	out, err := runGit(ctx, path, nil, "diff", "--name-only", "--diff-filter=U", "-z")
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
func hasMergeHead(ctx context.Context, path string) (bool, error) {
	_, err := runGit(ctx, path, nil, "rev-parse", "--verify", "-q", "MERGE_HEAD")
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

// resolvePrompt frames the task for the agent: edit every listed conflicted file to its correct
// resolved state, with no leftover conflict markers — and nothing else. It never asks the agent to
// stage or commit (a sandboxed agent such as codex's workspace-write cannot write inside .git anyway);
// finalizeAgentResolution does that once the agent exits.
func resolvePrompt(plan *MergePlan, conflictPaths []string) string {
	return fmt.Sprintf(
		"The git worktree at %s has a real merge conflict (MERGE_HEAD is set there), merging branch %q "+
			"(target commit %s) into it. Resolve every conflict marker in exactly these files, and only these "+
			"files: %v. Edit each file's contents to the correct resolved state. Do NOT run `git add`, "+
			"`git commit`, `git merge --abort`, or any other git command — a separate controller stages and "+
			"commits your edits once you exit. Do not touch any other git checkout or any file outside this "+
			"list. When you are done, every listed file must contain no lines starting with <<<<<<<, =======, "+
			"or >>>>>>>.",
		plan.WorktreePath, plan.TargetRef, plan.TargetOID, conflictPaths)
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
