package worktree

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// withWorktreeLease runs fn under the lease serializing resolve, land and clean per worktree (before the land lock).
func withWorktreeLease(repo *Repo, name string, fn func() error) error {
	leasePath := operationLeasePath(repo.Main, name)
	// TryFileLock does not create the lease dir, which may not exist yet on a first resolve.
	if err := os.MkdirAll(filepath.Dir(leasePath), 0o755); err != nil {
		return err
	}
	// one try, no wait: ran=false means the lease is held elsewhere
	ran, err := paths.TryFileLock(leasePath, 0, fn)
	if err != nil {
		return err
	}
	if !ran {
		return fmt.Errorf("worktree %q: another land, resolve, or clean operation is already active", name)
	}
	return nil
}

// LandWaitEnv names who runs a land (the lane-land hook sets it); the lease file records it for a waiting land.
const LandWaitEnv = "HARNESS_LAND_BY"

// LaneLanded reports whether name's worktree is gone and its ledger was closed by a land (not a clean).
func LaneLanded(repo *Repo, name string) bool {
	dest, err := resolveWorktreeBounded(repo, name)
	if err != nil {
		return false
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		return false
	}
	data, err := os.ReadFile(ledgerPath(repo.Main, name, time.Time{}))
	if err != nil {
		return false
	}
	return closedByLand(string(data))
}

// closedByLand reports whether the ledger is closed and its latest Closed section, after the latest Opened, says land.
func closedByLand(text string) bool {
	i := strings.LastIndex(text, "\n## Closed\n")
	if i < 0 || i < strings.LastIndex(text, "\n## Opened\n") || ledgerField(text, "Status") != "closed" {
		return false
	}
	return ledgerField(text[i:], "Via") == "land"
}

// WithLandLease is withWorktreeLease that waits (maxLandWait or ctx); fn gets the holder it waited on, "" if none.
func WithLandLease(ctx context.Context, repo *Repo, name string, log io.Writer, fn func(waitedOn string) error) error {
	leasePath := operationLeasePath(repo.Main, name)
	if err := os.MkdirAll(filepath.Dir(leasePath), 0o755); err != nil {
		return err
	}
	who := os.Getenv(LandWaitEnv)
	if who == "" {
		who = "manual land"
	}
	var waitedOn string
	deadline := nowFn().Add(maxLandWait)
	for {
		ran, err := paths.TryFileLock(leasePath, 0, func() error {
			// attribution only, in its own file so a reader never sees the lock file truncated
			_ = paths.AtomicWrite(leasePath+".who", []byte(strconv.Itoa(os.Getpid())+"\n"+who+"\n"), 0o600)
			return fn(waitedOn)
		})
		if err != nil || ran {
			return err
		}
		if waitedOn == "" {
			var pid string
			pid, waitedOn = leaseHolder(leasePath + ".who")
			fmt.Fprintf(log, "land %s: waiting for the active land (pid %s, %s)\n", name, pid, waitedOn)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if nowFn().After(deadline) {
			return fmt.Errorf("worktree %q: another land, resolve, or clean operation is still active after %s", name, maxLandWait)
		}
		sleepFn(pollInterval)
	}
}

// leaseHolder reads the pid and who the holder recorded; "?" and "another operation" when unrecorded.
func leaseHolder(whoPath string) (pid, who string) {
	data, err := os.ReadFile(whoPath)
	if err != nil {
		return "?", "another operation"
	}
	pid, who, _ = strings.Cut(strings.TrimSpace(string(data)), "\n")
	if pid == "" || who == "" {
		return "?", "another operation"
	}
	return pid, who
}

// Bounded-wait knobs and the agent-dispatch seam; tests shrink them so the poll loop runs in milliseconds.
var (
	nowFn          = time.Now
	sleepFn        = time.Sleep
	pollInterval   = 2 * time.Second
	maxResolveWait = 10 * time.Minute
	// maxLandWait matches the lane-land hook's land timeout (laneLandTime).
	maxLandWait = 45 * time.Minute
	// resolveWaitDelay bounds how long a killed agent's orphaned children may hold its output pipes open.
	resolveWaitDelay = 5 * time.Second
)

// askEnvelope is the subset of wire.Envelope (`claude-code agents ask --format json`) that resolve reads.
type askEnvelope struct {
	Answer         string `json:"answer"`
	Fallback       string `json:"fallback_from,omitempty"`
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
	// belt-and-suspenders over ask's own --timeout
	runCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, "agents", "ask", "--agent", agentName, "--workdir", workDir,
		"--tools", "--fallback", "none", "--timeout", timeout.String(), "--format", "json", "-")
	cmd.Stdin = strings.NewReader(prompt)
	cmd.WaitDelay = resolveWaitDelay
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

// resolveWithAgent drives one agent resolution, taking no lock (the caller holds the lease); op == nil runs fresh.
func resolveWithAgent(ctx context.Context, repo *Repo, plan *LandPlan, agentName string, op *Operation) (*Operation, *LandResult, error) {
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
		// merge-tree's prediction didn't reproduce; nothing to hand the agent
		return nil, materialized, nil
	}
	conflictPaths, err := unmergedPaths(ctx, plan.WorktreePath)
	if err != nil {
		return nil, nil, abortMaterialized(plan.WorktreePath, err)
	}
	op = newResolveOperation(repo, plan, agentName, conflictPaths)
	if err := SaveOperation(op); err != nil {
		return nil, nil, abortMaterialized(plan.WorktreePath, err)
	}
	return runAndFinalize(ctx, op, plan, resolvePrompt(plan, conflictPaths))
}

func newResolveOperation(repo *Repo, plan *LandPlan, agentName string, conflictPaths []string) *Operation {
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
func runAndFinalize(ctx context.Context, op *Operation, plan *LandPlan, prompt string) (*Operation, *LandResult, error) {
	res, runErr := runResolveAgent(ctx, op.Agent, prompt, plan.WorktreePath, maxResolveWait)
	if runErr != nil {
		return failOperation(op, fmt.Errorf("resolve: agent %s failed: %w", op.Agent, runErr))
	}
	if res.Fallback != "" {
		return failOperation(op, fmt.Errorf(
			"resolve: requested agent %q fell back to %q (%s) — refusing a silently different model", op.Agent, res.Fallback, res.FallbackReason))
	}
	// the one receipt resume is allowed to trust instead of re-running the agent
	op.AgentExitOK = true
	// A failed receipt save must not abort: the merge holds the agent's edits and the next checkpoint saves again.
	saveErr := SaveOperation(op)
	op, merged, err := finalizeAgentResolution(ctx, op, plan.WorktreePath)
	if saveErr != nil && err != nil {
		err = errors.Join(fmt.Errorf("resolve: save agent-exit receipt: %w", saveErr), err)
	}
	return op, merged, err
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
func materializeConflict(ctx context.Context, plan *LandPlan) (*LandResult, error) {
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
		return &LandResult{Phase: "integrate", BeforeOID: before, AfterOID: after}, nil
	}
	mergeHead, err := hasMergeHead(plan.WorktreePath)
	if err != nil {
		return nil, err
	}
	if !mergeHead {
		return nil, fmt.Errorf("git merge inside worktree: %w (%s)", mergeErr, firstNonEmpty(gitErrorDetail(mergeErr), string(out)))
	}
	return &LandResult{Phase: "conflict", BeforeOID: before}, nil
}

// defaultMarkerSize is git's conflict-marker width when a path sets no conflict-marker-size attribute.
const defaultMarkerSize = 7

// isConflictMarker matches a '<'/'>' marker of width size, alone or before a space; a bare '=' run may be a heading.
func isConflictMarker(line string, size int) bool {
	line = strings.TrimSuffix(line, "\r")
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
			// tracked as resolved but no longer on disk (e.g. deleted) — nothing to scan
			continue
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
	for _, args := range [][]string{{"--cached", tree}, nil} {
		diffArgs := append([]string{"diff", "--name-only", "--no-renames", "--ignore-submodules=none", "-z"}, args...)
		diff, err := runGit(ctx, worktreePath, nil, diffArgs...)
		if err != nil {
			return nil, fmt.Errorf("git diff in %s: %w (%s)", worktreePath, err, gitErrorDetail(err))
		}
		for _, f := range textutil.SplitNUL(string(diff)) {
			if !seen[f] {
				seen[f] = true
				result = append(result, f)
			}
		}
	}
	return result, nil
}

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
func finalizeAgentResolution(ctx context.Context, op *Operation, worktreePath string) (*Operation, *LandResult, error) {
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

func markResolved(ctx context.Context, op *Operation, worktreePath string) (*Operation, *LandResult, error) {
	head, err := headOfContext(ctx, worktreePath)
	if err != nil {
		return failOperation(op, err)
	}
	op.Phase = "resolved"
	if err := SaveOperation(op); err != nil {
		return op, nil, fmt.Errorf("resolve: save resolved checkpoint: %w", err)
	}
	return op, &LandResult{Phase: "integrate", BeforeOID: op.OriginalSourceOID, AfterOID: head}, nil
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
func pollUntilResolved(ctx context.Context, op *Operation, worktreePath string) (*Operation, *LandResult, error) {
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

func unmergedPaths(ctx context.Context, path string) ([]string, error) {
	out, err := runGit(ctx, path, nil, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, fmt.Errorf("git diff --diff-filter=U in %s: %w", path, err)
	}
	return textutil.SplitNUL(string(out)), nil
}

func hasMergeHead(path string) (bool, error) {
	gitDir, err := gitDirFromPath(path)
	if err != nil {
		return false, err
	}
	return markerExists(gitDir, "MERGE_HEAD")
}

func failOperation(op *Operation, err error) (*Operation, *LandResult, error) {
	return endOperation(op, "failed", err)
}

// endOperation saves op at a terminal phase and returns err, joined with any save error so a stale file is reported.
func endOperation(op *Operation, phase string, err error) (*Operation, *LandResult, error) {
	op.Phase = phase
	if serr := SaveOperation(op); serr != nil {
		err = errors.Join(err, fmt.Errorf("resolve: save %s checkpoint: %w", phase, serr))
	}
	return op, nil, err
}

// resolvePrompt has the agent edit the conflicted files only; a sandboxed agent cannot write .git.
func resolvePrompt(plan *LandPlan, conflictPaths []string) string {
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

func ValidResolveAgent(a string) bool { return a == "" || slices.Contains(ResolveAgents(), a) }

// Operation is the durable record of one agent resolution; OIDs are pinned before the agent runs so resume revalidates.
type Operation struct {
	Schema   int `json:"schema"`
	ID       string
	Repo     string
	Worktree string
	// the target branch pinned at resolve start; resume must match both this and TargetOID
	TargetRef         string
	TargetOID         string
	OriginalSourceOID string
	// paths the agent was handed; finalizeAgentResolution never stages/commits outside these
	ConflictPaths []string
	// "resolving" | "resolved" | "failed" | "timeout" | "cancelled"
	Phase     string
	StartedAt time.Time
	Deadline  time.Time
	Agent     string
	// true only once the agent process itself returned success with no fallback
	AgentExitOK bool
}

// operationsDir holds one JSON file per repo+worktree under the harness's existing runtime state dir.
func operationsDir() string {
	return paths.MoveLegacy(filepath.Join(paths.StateDir(), "worktree-merge-ops"), filepath.Join(paths.StateDir(), "worktree-land-ops"))
}

// operationPath is deterministic from repo+name, so --resume needs no id lookup.
func operationPath(repoMain, name string) string {
	slug := paths.Slug(filepath.Base(filepath.Clean(repoMain))+"-"+name, "._-", 0, "op")
	// the slug alone collides across repos/case
	sum := sha256.Sum256([]byte(filepath.Clean(repoMain) + "\x00" + name))
	return filepath.Join(operationsDir(), slug+"-"+hex.EncodeToString(sum[:4])+".json")
}

// operationLeasePath is the lock withWorktreeLease holds; not ".lock", the suffix paths.WithFileLock owns.
func operationLeasePath(repoMain, name string) string {
	return operationPath(repoMain, name) + ".lease"
}

// SaveOperation atomically writes op, overwriting any prior state saved for the same repo+worktree.
func SaveOperation(op *Operation) error {
	op.Schema = operationSchema
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return fmt.Errorf("save operation: %w", err)
	}
	return paths.AtomicWrite(operationPath(op.Repo, op.Worktree), data, 0o600)
}

// loadOperation reads the saved operation; a missing file surfaces as the plain *os.PathError for --resume to report.
func loadOperation(repo *Repo, name string) (*Operation, error) {
	data, err := os.ReadFile(operationPath(repo.Main, name))
	if err != nil {
		return nil, err
	}
	var op Operation
	if _, err := schemas.Decode(operationFormat, data, &op); err != nil {
		return nil, fmt.Errorf("load operation: %w", err)
	}
	return &op, nil
}

// operationSchema versions worktree-land-op.
const operationSchema = 1

var operationFormat = schemas.Register(schemas.Format{
	Name: "worktree-land-op", Owner: "internal/worktree", Kind: schemas.Record, Version: operationSchema,
	Glob:    func() string { return filepath.Join(operationsDir(), "*.json") },
	New:     func() any { return new(Operation) },
	Keep:    "resume record for an interrupted agent resolve",
	Migrate: schemas.SameShape,
})

// CheckResumable mirrors the pre-agent checks of the real --resume so a dry run fails where that would.
func CheckResumable(repo *Repo, name string) error {
	_, _, err := loadResumable(repo, name)
	return err
}

// loadResumable loads name's saved operation and its worktree path, failing if either is missing or foreign to repo.
func loadResumable(repo *Repo, name string) (op *Operation, worktreePath string, err error) {
	op, err = loadOperation(repo, name)
	if err != nil {
		return nil, "", fmt.Errorf("no in-progress --resolve-agent operation found for %q: %w", name, err)
	}
	if op.Repo != repo.Main {
		return nil, "", fmt.Errorf("operation %s belongs to %s, not %s", op.ID, op.Repo, repo.Main)
	}
	if op.Worktree != name {
		return nil, "", fmt.Errorf("operation %s records worktree %q, not %q", op.ID, op.Worktree, name)
	}
	if worktreePath, err = checkWorktreeName(repo, name); err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(worktreePath); err != nil {
		return nil, "", fmt.Errorf("worktree %q not found at %s: %w", op.Worktree, worktreePath, err)
	}
	return op, worktreePath, nil
}
