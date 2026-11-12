package worktree

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
)

// WithWorktreeLease runs fn under the lease serializing resolve, merge and clean per worktree (before the merge lock).
func WithWorktreeLease(repo *Repo, name string, fn func() error) error {
	leasePath := operationLeasePath(repo.Main, name)
	// TryFileLock does not create the lease dir, which may not exist yet on a first resolve.
	if err := os.MkdirAll(filepath.Dir(leasePath), 0o755); err != nil {
		return err
	}
	ran, err := paths.TryFileLock(leasePath, 0, fn) // one try, no wait: ran=false means the lease is held elsewhere
	if err != nil {
		return err
	}
	if !ran {
		return fmt.Errorf("worktree %q: another merge, resolve, or clean operation is already active", name)
	}
	return nil
}

// Bounded-wait knobs and the agent-dispatch seam; tests shrink them so the poll loop runs in milliseconds.
var (
	nowFn          = time.Now
	sleepFn        = time.Sleep
	pollInterval   = 2 * time.Second
	maxResolveWait = 10 * time.Minute
)

// askEnvelope mirrors the JSON `claude-code agents ask --format json` prints: agent.Result's fields plus "error".
type askEnvelope struct {
	Answer         string `json:"answer"`
	Fallback       string `json:"fallback,omitempty"`
	FallbackReason string `json:"fallback_reason,omitempty"`
	Error          *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// runResolveAgent runs `ask --fallback none` once in workDir (codex never silently becomes claude); no retry.
var runResolveAgent = func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
	bin, err := claudeCodeBin()
	if err != nil {
		return askEnvelope{}, fmt.Errorf("resolve: locate claude-code binary: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second) // belt-and-suspenders over ask's own --timeout
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, "agents", "ask", "--agent", agentName, "--workdir", workDir,
		"--tools", "--fallback", "none", "--timeout", timeout.String(), "--format", "json", "-")
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, runErr := cmd.Output()

	var env askEnvelope
	if jsonErr := json.Unmarshal(stdout, &env); jsonErr != nil {
		if runErr != nil {
			return askEnvelope{}, fmt.Errorf("claude-code agents ask: %w (%s)", runErr, strings.TrimSpace(stderr.String()))
		}
		return askEnvelope{}, fmt.Errorf("claude-code agents ask: unparseable output: %w", jsonErr)
	}
	if env.Error != nil {
		return env, fmt.Errorf("claude-code agents ask: %s", env.Error.Message)
	}
	if runErr != nil {
		return env, fmt.Errorf("claude-code agents ask: %w (%s)", runErr, strings.TrimSpace(stderr.String()))
	}
	return env, nil
}

// claudeCodeBin resolves the binary to self-exec: os.Executable, else PATH (e.g. under `go run`).
func claudeCodeBin() (string, error) {
	if exe, err := os.Executable(); err == nil {
		return exe, nil
	}
	return exec.LookPath("claude-code")
}

// resolutionState is checkResolution's verdict: pending, a verified clean commit, or invalid (abort/stale).
type resolutionState int

const (
	resolutionPending resolutionState = iota
	resolutionResolved
	resolutionInvalid
)

// resolveWithAgent drives one agent resolution under the lease; op == nil runs fresh, else resumes by polling.
func resolveWithAgent(ctx context.Context, repo *Repo, plan *MergePlan, agentName string, op *Operation) (*Operation, *MergeResult, error) {
	if op != nil {
		if !op.AgentExitOK {
			return nil, nil, fmt.Errorf(
				"resolve: operation %s has no persisted successful agent-exit receipt (phase %q) — the agent may still be running or never completed; refusing to resume", op.ID, op.Phase)
		}
		return pollUntilResolved(ctx, op, plan.WorktreePath)
	}
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
	op = newResolveOperation(repo, plan, agentName, conflictPaths)
	if err := SaveOperation(op); err != nil {
		return nil, nil, abortMaterialized(plan.WorktreePath, err)
	}
	return runAndFinalize(ctx, op, plan, resolvePrompt(plan, conflictPaths))
}

// newResolveOperation is the "resolving" record for a fresh agent resolution, pinned to plan's OIDs.
func newResolveOperation(repo *Repo, plan *MergePlan, agentName string, conflictPaths []string) *Operation {
	op := &Operation{
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
	return op
}

// runAndFinalize runs op's agent once, records the exit receipt, then verifies and commits its edits.
func runAndFinalize(ctx context.Context, op *Operation, plan *MergePlan, prompt string) (*Operation, *MergeResult, error) {
	res, runErr := runResolveAgent(ctx, op.Agent, prompt, plan.WorktreePath, maxResolveWait)
	if runErr != nil {
		return failOperation(op, fmt.Errorf("resolve: agent %s failed: %w", op.Agent, runErr))
	}
	if res.Fallback != "" {
		return failOperation(op, fmt.Errorf(
			"resolve: requested agent %q fell back to %q (%s) — refusing a silently different model", op.Agent, res.Fallback, res.FallbackReason))
	}
	op.AgentExitOK = true // the one receipt resume is allowed to trust instead of re-running the agent
	if err := SaveOperation(op); err != nil {
		return nil, nil, err
	}
	return finalizeAgentResolution(ctx, op, plan.WorktreePath)
}

// abortMaterialized undoes a merge whose operation file failed to save (no resume record); returns saveErr.
func abortMaterialized(worktreePath string, saveErr error) error {
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	if out, err := runGit(ctx, worktreePath, nil, "merge", "--abort"); err != nil {
		return errors.Join(saveErr, fmt.Errorf("git merge --abort in %s failed, abort by hand: %w (%s)",
			worktreePath, err, firstNonEmpty(gitErrorDetail(err), string(out))))
	}
	return fmt.Errorf("resolve: save operation, merge aborted: %w", saveErr)
}

// materializeConflict makes the predicted conflict real in the worktree; a clean merge returns Phase "integrate".
func materializeConflict(ctx context.Context, plan *MergePlan) (*MergeResult, error) {
	if issue, err := checkCheckoutState(ctx, plan.WorktreePath, plan.WorktreeGitDir, isDirty); err != nil {
		return nil, err
	} else if issue != nil {
		return nil, fmt.Errorf("resolve: %s: %v", issue.Code, issue.Detail)
	}
	before, err := headOfContext(ctx, plan.WorktreePath)
	if err != nil {
		return nil, err
	}
	out, mergeErr := runGit(ctx, plan.WorktreePath, nil, "merge", "--no-edit", plan.TargetOID)
	if mergeErr == nil {
		after, err := headOfContext(ctx, plan.WorktreePath)
		if err != nil {
			return nil, err
		}
		return &MergeResult{Phase: "integrate", BeforeOID: before, AfterOID: after}, nil
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

// defaultMarkerSize is git's conflict-marker width when a path sets no conflict-marker-size attribute.
const defaultMarkerSize = 7

// isConflictMarker matches a git marker line of width size: '=' alone, or '<'/'>' alone or followed by a space.
func isConflictMarker(line string, size int) bool {
	line = strings.TrimSuffix(line, "\r")
	if line == strings.Repeat("=", size) {
		return true
	}
	for _, c := range []string{"<", ">"} {
		marker := strings.Repeat(c, size)
		if line == marker || strings.HasPrefix(line, marker+" ") {
			return true
		}
	}
	return false
}

// markerSizes maps each path to its conflict-marker width: the conflict-marker-size attribute, else the default.
func markerSizes(ctx context.Context, worktreePath string, paths []string) (map[string]int, error) {
	sizes := make(map[string]int, len(paths))
	for _, p := range paths {
		sizes[p] = defaultMarkerSize
	}
	if len(paths) == 0 {
		return sizes, nil
	}
	args := append([]string{"check-attr", "-z", "conflict-marker-size", "--"}, paths...)
	out, err := runGit(ctx, worktreePath, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git check-attr in %s: %w (%s)", worktreePath, err, gitErrorDetail(err))
	}
	// -z output is NUL-separated "path, attribute, value" triples; a non-numeric value ("unspecified") keeps the default.
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+2 < len(fields); i += 3 {
		if n, convErr := strconv.Atoi(fields[i+2]); convErr == nil && n > 0 {
			sizes[fields[i]] = n
		}
	}
	return sizes, nil
}

// conflictMarkersClear scans each path at its marker width (default 7); badPath names one offender when clean is false.
func conflictMarkersClear(ctx context.Context, worktreePath string, paths []string) (clean bool, badPath string, err error) {
	sizes, err := markerSizes(ctx, worktreePath, paths)
	if err != nil {
		return false, "", err
	}
	for _, p := range paths {
		data, readErr := os.ReadFile(filepath.Join(worktreePath, p))
		if os.IsNotExist(readErr) {
			continue // tracked as resolved but no longer on disk (e.g. deleted) — nothing to scan
		}
		if readErr != nil {
			return false, "", fmt.Errorf("scan %s for conflict markers: %w", p, readErr)
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			if isConflictMarker(line, sizes[p]) {
				return false, p, nil
			}
		}
	}
	return true, "", nil
}

// changedPaths lists TRACKED changes only: untracked tooling residue is never staged, and Clean still keeps the tree.
func changedPaths(ctx context.Context, worktreePath string) ([]string, error) {
	out, err := runGit(ctx, worktreePath, nil, "status", "--porcelain", "--ignore-submodules=none", "--untracked-files=no")
	if err != nil {
		return nil, fmt.Errorf("git status in %s: %w", worktreePath, err)
	}
	var result []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		result = append(result, strings.TrimSpace(line[3:]))
	}
	return result, nil
}

// agentChangedPaths lists tracked paths differing from the clean merge of the pinned OIDs (not auto-merged files).
func agentChangedPaths(ctx context.Context, op *Operation, worktreePath string) ([]string, error) {
	out, err := runGit(ctx, worktreePath, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z",
		op.OriginalSourceOID, op.TargetOID)
	if code, isExit := asExitErr(err); err != nil && (!isExit || code != 1) {
		return nil, fmt.Errorf("git merge-tree in %s: %w (%s)", worktreePath, err, gitErrorDetail(err))
	}
	tree, _, _ := strings.Cut(string(out), "\x00")
	seen := map[string]bool{}
	var result []string
	for _, args := range [][]string{{"--cached", tree}, nil} { // index vs clean merge, then worktree vs index
		diffArgs := append([]string{"diff", "--name-only", "--no-renames", "--ignore-submodules=none", "-z"}, args...)
		diff, err := runGit(ctx, worktreePath, nil, diffArgs...)
		if err != nil {
			return nil, fmt.Errorf("git diff in %s: %w (%s)", worktreePath, err, gitErrorDetail(err))
		}
		for f := range strings.SplitSeq(string(diff), "\x00") {
			if f != "" && !seen[f] {
				seen[f] = true
				result = append(result, f)
			}
		}
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

// finalizeAgentResolution checks markers are gone and nothing else changed, then add + commit; a failure fails the op.
func finalizeAgentResolution(ctx context.Context, op *Operation, worktreePath string) (*Operation, *MergeResult, error) {
	clean, badPath, err := conflictMarkersClear(ctx, worktreePath, op.ConflictPaths)
	if err != nil {
		return failOperation(op, err)
	}
	if !clean {
		return failOperation(op, fmt.Errorf("resolve: conflict markers remain in %q after the agent exited — refusing to stage or commit", badPath))
	}
	if err := agentEditsInScope(ctx, op, worktreePath); err != nil {
		return failOperation(op, err)
	}

	addArgs := append([]string{"add", "--"}, op.ConflictPaths...)
	if out, err := runGit(ctx, worktreePath, nil, addArgs...); err != nil {
		return failOperation(op, fmt.Errorf("git add in %s: %w (%s)", worktreePath, err, firstNonEmpty(gitErrorDetail(err), string(out))))
	}
	if out, err := runGit(ctx, worktreePath, nil, "commit", "--no-edit"); err != nil {
		return failOperation(op, fmt.Errorf("git commit in %s: %w (%s)", worktreePath, err, firstNonEmpty(gitErrorDetail(err), string(out))))
	}

	state, reason, err := checkResolution(ctx, op, worktreePath)
	if err != nil {
		return failOperation(op, err)
	}
	if state != resolutionResolved {
		return failOperation(op, fmt.Errorf("resolve: %s", firstNonEmpty(reason, "commit did not produce a resolved state")))
	}
	return markResolved(ctx, op, worktreePath)
}

// markResolved checkpoints op as resolved at the worktree's HEAD and returns the integrate result.
func markResolved(ctx context.Context, op *Operation, worktreePath string) (*Operation, *MergeResult, error) {
	head, err := headOfContext(ctx, worktreePath)
	if err != nil {
		return failOperation(op, err)
	}
	op.Phase = "resolved"
	if err := SaveOperation(op); err != nil {
		return op, nil, fmt.Errorf("resolve: save resolved checkpoint: %w", err)
	}
	return op, &MergeResult{Phase: "integrate", BeforeOID: op.OriginalSourceOID, AfterOID: head}, nil
}

// agentEditsInScope errors when the agent changed any tracked path beyond op's conflicted paths.
func agentEditsInScope(ctx context.Context, op *Operation, worktreePath string) error {
	changed, err := agentChangedPaths(ctx, op, worktreePath)
	if err != nil {
		return err
	}
	if extra := outsideConflictPaths(changed, op.ConflictPaths); len(extra) > 0 {
		return fmt.Errorf("resolve: agent changed %v outside the conflicted paths %v — refusing to stage or commit", extra, op.ConflictPaths)
	}
	return nil
}

// pollUntilResolved is the --resume path: it polls checkResolution until resolved, invalid, canceled or op.Deadline.
func pollUntilResolved(ctx context.Context, op *Operation, worktreePath string) (*Operation, *MergeResult, error) {
	for {
		if err := ctx.Err(); err != nil {
			return endOperation(op, "cancelled", fmt.Errorf("resolve: cancelled: %w", err))
		}
		state, reason, err := checkResolution(ctx, op, worktreePath)
		if err != nil {
			return failOperation(op, err)
		}
		switch state {
		case resolutionResolved:
			return markResolved(ctx, op, worktreePath)
		case resolutionInvalid:
			return failOperation(op, fmt.Errorf("resolve: %s", reason))
		}
		if !nowFn().Before(op.Deadline) {
			return endOperation(op, "timeout", fmt.Errorf("resolve: no clean, committed resolution within %s of starting", maxResolveWait))
		}
		sleepFn(pollInterval)
	}
}

// checkResolution checks git state vs op's pinned OIDs: pending mid-merge, invalid if aborted or stale, else resolved.
func checkResolution(ctx context.Context, op *Operation, worktreePath string) (resolutionState, string, error) {
	pending, err := resolutionMidMerge(ctx, worktreePath)
	if err != nil {
		return resolutionPending, "", err
	}
	if pending {
		return resolutionPending, "", nil
	}

	tracked, err := changedPaths(ctx, worktreePath)
	if err != nil {
		return resolutionPending, "", err
	}
	if len(tracked) > 0 {
		return resolutionInvalid, "worktree has uncommitted changes outside a merge, not a committed resolution", nil
	}

	head, err := headOfContext(ctx, worktreePath)
	if err != nil {
		return resolutionPending, "", err
	}
	if head == "" {
		return resolutionPending, "", fmt.Errorf("resolve: could not resolve HEAD in %s", worktreePath)
	}
	if head == op.OriginalSourceOID {
		return resolutionInvalid, "HEAD is unchanged since the agent started — the merge was aborted, not resolved", nil
	}
	state, reason, err := checkPinnedAncestry(ctx, op, worktreePath, head)
	if err != nil || state != resolutionResolved {
		return state, reason, err
	}
	return verifyResolvedContent(ctx, op, worktreePath)
}

// verifyResolvedContent applies finalize's content checks to a commit, so resume matches a fresh finalize.
func verifyResolvedContent(ctx context.Context, op *Operation, worktreePath string) (resolutionState, string, error) {
	clean, badPath, err := conflictMarkersClear(ctx, worktreePath, op.ConflictPaths)
	if err != nil {
		return resolutionPending, "", err
	}
	if !clean {
		return resolutionInvalid, fmt.Sprintf("conflict markers remain in %q in the committed resolution", badPath), nil
	}
	if err := agentEditsInScope(ctx, op, worktreePath); err != nil {
		return resolutionPending, "", err
	}
	return resolutionResolved, "", nil
}

// resolutionMidMerge reports whether MERGE_HEAD is still set or any path is still unmerged in worktreePath.
func resolutionMidMerge(ctx context.Context, worktreePath string) (bool, error) {
	unmerged, err := unmergedPaths(ctx, worktreePath)
	if err != nil {
		return false, err
	}
	mergeHead, err := hasMergeHead(worktreePath)
	if err != nil {
		return false, err
	}
	return mergeHead || len(unmerged) > 0, nil
}

// checkPinnedAncestry requires both of op's pinned commits to be ancestors of head, else the resolution is invalid.
func checkPinnedAncestry(ctx context.Context, op *Operation, worktreePath, head string) (resolutionState, string, error) {
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

// unmergedPaths reads `git diff --name-only --diff-filter=U -z`; empty means every handed conflict is resolved.
func unmergedPaths(ctx context.Context, path string) ([]string, error) {
	out, err := runGit(ctx, path, nil, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, fmt.Errorf("git diff --diff-filter=U in %s: %w", path, err)
	}
	var paths []string
	for f := range strings.SplitSeq(string(out), "\x00") {
		if f != "" {
			paths = append(paths, f)
		}
	}
	return paths, nil
}

// hasMergeHead reports whether path's own git-dir still has an in-progress merge recorded.
func hasMergeHead(path string) (bool, error) {
	gitDir, err := gitDirFromPath(path)
	if err != nil {
		return false, err
	}
	return markerExists(gitDir, "MERGE_HEAD")
}

// failOperation marks op failed and re-saves it; the file is never cleaned up, so resume decides what to do next.
func failOperation(op *Operation, err error) (*Operation, *MergeResult, error) {
	return endOperation(op, "failed", err)
}

// endOperation saves op at a terminal phase and returns err, joined with any save error so a stale file is reported.
func endOperation(op *Operation, phase string, err error) (*Operation, *MergeResult, error) {
	op.Phase = phase
	if serr := SaveOperation(op); serr != nil {
		err = errors.Join(err, fmt.Errorf("resolve: save %s checkpoint: %w", phase, serr))
	}
	return op, nil, err
}

// resolvePrompt has the agent edit the conflicted files only; a sandboxed agent cannot write .git.
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

// newOperationID is a short random tag; collisions are harmless since the file path is keyed by repo+worktree.
func newOperationID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("op-%d", nowFn().UnixNano())
	}
	return "op-" + hex.EncodeToString(b)
}

// ResolveAgents are the --resolve-agent values: registered roster agents whose tools run natively.
func ResolveAgents() []string {
	return slices.DeleteFunc(agentid.All(), func(a string) bool {
		s, ok := agent.LookupSchema(a)
		return !ok || s.CLI.Caps.Tools != agent.Native
	})
}

// ValidResolveAgent reports whether a is a --resolve-agent value merge accepts ("" means none).
func ValidResolveAgent(a string) bool { return a == "" || slices.Contains(ResolveAgents(), a) }
