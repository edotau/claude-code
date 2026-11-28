package worktree

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// resolveFixtureConflictSetup builds a same-file divergent-edit conflict and returns its one-IssueConflict plan.
func resolveFixtureConflictSetup(t *testing.T) (repo *Repo, main, wt string, plan *LandPlan) {
	t.Helper()
	buildConflictTemplate(t)
	base := paths.Canon(t.TempDir())
	name := filepath.Base(base)
	main = filepath.Join(base, name)
	repo = &Repo{Main: main, Root: filepath.Join(base, "worktrees", name)}
	wt = filepath.Join(repo.Root, "feature")
	copyWorktreeAs(t, conflictFixture.main, conflictFixture.wt, main, wt, "feature", "feature", "", fixture.base)
	cp := *conflictFixture.plan
	cp.CommonDir = filepath.Join(main, ".git")
	cp.MainGitDir = cp.CommonDir
	cp.WorktreeGitDir = filepath.Join(cp.CommonDir, "worktrees", "feature")
	cp.WorktreePath = wt
	cp.Issues = slices.Clone(conflictFixture.plan.Issues)
	for i := range cp.Issues {
		cp.Issues[i].Paths = slices.Clone(cp.Issues[i].Paths)
	}
	return repo, main, wt, &cp
}

// resolveFixtureFakeClock swaps in an in-memory clock and short poll/wait bounds, restored on cleanup.
func resolveFixtureFakeClock(t *testing.T, maxWait time.Duration) {
	t.Helper()
	origNow, origSleep, origPoll, origMax := nowFn, sleepFn, pollInterval, maxResolveWait
	fake := time.Now()
	nowFn = func() time.Time { return fake }
	sleepFn = func(d time.Duration) { fake = fake.Add(d) }
	pollInterval = time.Millisecond
	maxResolveWait = maxWait
	t.Cleanup(func() {
		nowFn, sleepFn, pollInterval, maxResolveWait = origNow, origSleep, origPoll, origMax
	})
}

// resolveFixtureFakeAgent stubs the agent seam resolveWithAgent calls, restored on cleanup.
func resolveFixtureFakeAgent(t *testing.T, fn func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error)) {
	t.Helper()
	orig := runResolveAgent
	runResolveAgent = fn
	t.Cleanup(func() { runResolveAgent = orig })
}

func TestHasMergeHeadUsesCheckoutLocalFile(t *testing.T) {
	t.Parallel()
	_, main, wt := preflightFixtureSetup(t)
	gitDir, err := gitDirFromPath(wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := hasMergeHead(wt)
	if err != nil || !got {
		t.Fatalf("linked checkout MERGE_HEAD exists despite invalid contents: got %v, %v", got, err)
	}
	got, err = hasMergeHead(main)
	if err != nil || got {
		t.Fatalf("main must not inherit the linked checkout's MERGE_HEAD: got %v, %v", got, err)
	}
}

func requireMergeHead(t *testing.T, wt string, want bool, why string) {
	t.Helper()
	mh, err := hasMergeHead(wt)
	if err != nil {
		t.Fatal(err)
	}
	if mh != want {
		t.Fatal(why)
	}
}

func requireFileText(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", filepath.Base(path), got, want)
	}
}

func requirePhase(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s.Phase = %q, want %q", name, got, want)
	}
}

func requireIntegrate(t *testing.T, result *LandResult, wantOID string) {
	t.Helper()
	if result == nil || result.Phase != "integrate" || result.AfterOID != wantOID {
		t.Fatalf("result = %+v, want integrate -> %s", result, wantOID)
	}
}

func resolveFixtureRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	landFixtureRun(t, dir, args...)
}

// A sandboxed agent can't write inside .git: it only edits, finalizeAgentResolution stages and commits.
func TestResolveWithAgentEditOnlyMarkersRemovedControllerCommits(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		// Only an edit — no git add/commit, matching the prompt's instruction to a sandboxed agent.
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return askEnvelope{}, nil
	})

	op, result, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("ResolveWithAgent: %v", err)
	}
	requireIntegrate(t, result, headOf(wt))
	requirePhase(t, "op", op.Phase, "resolved")
	requireMergeHead(t, wt, false, "MERGE_HEAD still set after the controller's own commit")
	requireFileText(t, filepath.Join(wt, "f.txt"), "resolved\n")
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

// Untracked tooling residue outside the conflicted paths is never staged and never blocks the commit.
func TestResolveWithAgentUntrackedResidueIgnoredAndPromotes(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Untracked tooling residue, never `git add`-ed by anything — not one of the conflicted paths.
		if err := os.WriteFile(filepath.Join(workDir, "hook.cache.json"), []byte(`{"residue":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return askEnvelope{}, nil
	})

	op, result, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("ResolveWithAgent: want the untracked residue to be ignored, got error: %v", err)
	}
	if result == nil || result.Phase != "integrate" {
		t.Fatalf("result = %+v, want a completed integrate", result)
	}
	if op.Phase != "resolved" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "resolved")
	}
	// The residue itself must be left exactly as the agent's tooling wrote it — never staged, never removed.
	requireFileText(t, filepath.Join(wt, "hook.cache.json"), `{"residue":true}`)
	staged, err := runGit(context.Background(), wt, nil, "status", "--porcelain", "--", "hook.cache.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(staged)), "??") {
		t.Fatalf("git status for hook.cache.json = %q, want it still untracked (\"??\"), never staged", staged)
	}
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

// Clean still refuses on untracked residue (--untracked-files=all), unlike the resolve commit scope.
func TestCleanRefusesWhileUntrackedResidueExists(t *testing.T) {
	landFixtureLockEnv(t)
	repo, _, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Preflight: issues=%v err=%v", plan.Issues, err)
	}
	if _, err := integrateTarget(context.Background(), plan); err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}
	if _, err := promoteOnly(context.Background(), repo, plan); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// Simulate tooling residue (e.g. a plugin hook cache) left behind after the merge already landed.
	if err := os.WriteFile(filepath.Join(wt, "hook.cache.json"), []byte(`{"residue":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanResult, err := Clean(repo, "feature", false)
	if err != nil {
		t.Fatalf("Clean: %v", err)
	}
	if cleanResult.Removed {
		t.Fatal("Clean: want it refused while untracked residue remains, not removed")
	}
	if !strings.Contains(cleanResult.Reason, "uncommitted") {
		t.Fatalf("cleanResult.Reason = %q, want it to name the uncommitted/dirty state", cleanResult.Reason)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree must survive a refused clean: %v", err)
	}
}

// Leftover markers: nothing is staged or committed, and the op file and edits survive for --resume.
func TestResolveWithAgentMarkersRemainFailsLikeTimeoutPreservingState(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	const markeredEdit = "<<<<<<< HEAD\nmain line\n=======\nfeature line\n>>>>>>> feature\n"
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte(markeredEdit), 0o644); err != nil {
			t.Fatal(err)
		}
		return askEnvelope{}, nil
	})

	op, result, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error — leftover conflict markers must never be staged or committed")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on a markers-remain failure", result)
	}
	if !strings.Contains(err.Error(), "conflict markers remain") {
		t.Fatalf("err = %q, want a markers-remain message", err)
	}
	requirePhase(t, "op", op.Phase, "failed")
	reloaded, loadErr := loadOperation(repo, "feature")
	if loadErr != nil {
		t.Fatalf("loadOperation after failure: %v", loadErr)
	}
	if reloaded.Phase != "failed" || reloaded.ID != op.ID {
		t.Fatalf("reloaded operation = %+v, want the preserved failed record", reloaded)
	}
	requireFileText(t, filepath.Join(wt, "f.txt"), markeredEdit)
	requireMergeHead(t, wt, true, "MERGE_HEAD was cleaned up after a markers-remain failure — no automatic cleanup is permitted")
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

// An agent edit outside the conflicted paths is refused even when the conflict itself was resolved.
func TestResolveWithAgentEditsUnrelatedFileRefused(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	// Tracked file: unlike untracked residue, a change to it must be refused.
	landFixtureWrite(t, wt, "tracked.txt", "original\n", "add tracked file")
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workDir, "tracked.txt"), []byte("should not change\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return askEnvelope{}, nil
	})

	op, result, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error — an edit outside the conflicted paths must be refused")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil when an out-of-scope edit is refused", result)
	}
	if !strings.Contains(err.Error(), "outside the conflicted paths") {
		t.Fatalf("err = %q, want the out-of-scope message", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	mh, mhErr := hasMergeHead(wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if !mh {
		t.Fatal("MERGE_HEAD was cleaned up despite the out-of-scope refusal — no automatic cleanup is permitted")
	}
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

// Regression: a clean file auto-merged from the target is not counted as an agent edit outside the conflict.
func TestResolveWithAgentAutoMergedFileAllowed(t *testing.T) {
	repo, main, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "f.txt", "feature line\n", "feature edits f")
	landFixtureWrite(t, main, "f.txt", "main line\n", "main edits f")
	landFixtureWrite(t, main, "n.txt", "new on main\n", "main adds n")
	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return askEnvelope{}, nil
	})

	op, result, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("ResolveWithAgent: %v", err)
	}
	if result == nil || op.Phase != "resolved" {
		t.Fatalf("result=%+v phase=%q, want a resolved merge", result, op.Phase)
	}
	if got, readErr := os.ReadFile(filepath.Join(wt, "n.txt")); readErr != nil || string(got) != "new on main\n" {
		t.Fatalf("n.txt = %q, %v, want the auto-merged file", got, readErr)
	}
}

// An agent's `git merge --abort` leaves nothing staged, so the controller's commit fails rather than resolving.
func TestResolveWithAgentCleanAbortNeverCommittedByController(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	sourceBefore := plan.SourceOID
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		resolveFixtureRunGit(t, workDir, "merge", "--abort")
		return askEnvelope{}, nil
	})

	op, _, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error for a clean abort")
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Fatalf("err = %q, want a commit-failure message (nothing left to stage after the abort)", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	if headOf(wt) != sourceBefore {
		t.Fatalf("worktree HEAD = %s, want unchanged %s after abort", headOf(wt), sourceBefore)
	}
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

// An agent-fabricated commit leaves nothing staged, so the fresh run fails and never promotes it.
func TestResolveWithAgentStaleMergeCommitNeverPromoted(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		resolveFixtureRunGit(t, workDir, "merge", "--abort")
		resolveFixtureRunGit(t, workDir, "commit", "--allow-empty", "-m", "stale leftover commit")
		return askEnvelope{}, nil
	})

	op, _, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want an error for a stale/unrelated commit")
	}
	if !strings.Contains(err.Error(), "commit") {
		t.Fatalf("err = %q, want a commit-failure message", err)
	}
	if op.Phase != "failed" {
		t.Fatalf("op.Phase = %q, want %q", op.Phase, "failed")
	}
	if ok, aerr := isAncestor(context.Background(), wt, plan.TargetOID, headOf(wt)); aerr == nil && ok {
		t.Fatal("fixture invariant broken: TargetOID must NOT be an ancestor of the stale commit")
	}
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

// A self-committing agent fails the fresh run, but --resume revalidates the committed state without re-invoking it.
func TestResolveWithAgentCommitsItselfFailsFreshButResumeSucceeds(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		resolveFixtureRunGit(t, workDir, "add", "f.txt")
		resolveFixtureRunGit(t, workDir, "commit", "--no-edit")
		return askEnvelope{}, nil
	})

	failedOp, result, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent (fresh): want an error — the controller's own commit has nothing staged")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on the fresh-run failure", result)
	}
	requirePhase(t, "failedOp", failedOp.Phase, "failed")
	if !failedOp.AgentExitOK {
		t.Fatal("failedOp.AgentExitOK = false, want true — the agent process itself exited cleanly")
	}
	resolvedHead := headOf(wt)

	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		t.Fatal("agent must never be re-invoked on --resume")
		return askEnvelope{}, errors.New("unreachable")
	})
	resumedOp, resumeResult, resumeErr := resolveWithAgent(context.Background(), repo, plan, "claude", failedOp)
	if resumeErr != nil {
		t.Fatalf("ResolveWithAgent (resume): %v", resumeErr)
	}
	requireIntegrate(t, resumeResult, resolvedHead)
	requirePhase(t, "resumedOp", resumedOp.Phase, "resolved")
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

// crashedOperation materializes plan's conflict and saves the "resolving" operation a controller crash leaves.
func crashedOperation(t *testing.T, repo *Repo, wt string, plan *LandPlan) *Operation {
	t.Helper()
	materialized, err := materializeConflict(context.Background(), plan)
	if err != nil {
		t.Fatalf("materializeConflict: %v", err)
	}
	requirePhase(t, "materialized", materialized.Phase, "conflict")
	op := &Operation{
		ID:                "op-crash-sim",
		Repo:              repo.Main,
		Worktree:          filepath.Base(wt),
		ConflictPaths:     []string{"f.txt"},
		TargetOID:         plan.TargetOID,
		OriginalSourceOID: plan.SourceOID,
		Phase:             "resolving",
		StartedAt:         time.Now(),
		Deadline:          time.Now().Add(10 * time.Minute),
		Agent:             "claude",
		// the agent itself returned success, fallback-free, before the crash
		AgentExitOK: true,
	}
	if err := SaveOperation(op); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	return op
}

// After a controller crash, --resume revalidates the already-committed merge without re-invoking the agent.
func TestResolveWithAgentResumeRevalidatesAndCompletes(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)

	op := crashedOperation(t, repo, wt, plan)

	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("resolved by agent before crash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolveFixtureRunGit(t, wt, "add", "f.txt")
	resolveFixtureRunGit(t, wt, "commit", "--no-edit")
	resolvedHead := headOf(wt)

	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		t.Fatal("agent must never be re-invoked on --resume")
		return askEnvelope{}, errors.New("unreachable")
	})

	gotOp, result, err := resolveWithAgent(context.Background(), repo, plan, op.Agent, op)
	if err != nil {
		t.Fatalf("ResolveWithAgent (resume): %v", err)
	}
	requireIntegrate(t, result, resolvedHead)
	requirePhase(t, "gotOp", gotOp.Phase, "resolved")
	reloaded, loadErr := loadOperation(repo, "feature")
	if loadErr != nil || reloaded.Phase != "resolved" {
		t.Fatalf("reloaded = %+v, err=%v, want a persisted resolved state", reloaded, loadErr)
	}
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

// promote stops with a retry hint when main advances between resolve and promote.
func TestResolveWithAgentThenPromoteStopsWhenTargetAdvanced(t *testing.T) {
	landFixtureLockEnv(t)
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		if err := os.WriteFile(filepath.Join(workDir, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return askEnvelope{}, nil
	})

	_, result, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("ResolveWithAgent: %v", err)
	}
	requirePhase(t, "result", result.Phase, "integrate")
	requireIntegrate(t, result, headOf(wt))

	// Production order: integrateTarget runs after resolution and pins plan.IntegratedOID.
	if _, err := integrateTarget(context.Background(), plan); err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}

	landFixtureWrite(t, main, "unrelated.txt", "main moved on\n", "main advances independently")
	advancedMain := headOf(main)

	promoted, promoteErr := promoteOnly(context.Background(), repo, plan)
	if promoteErr == nil {
		t.Fatal("promote: want an error — target advanced since the plan was pinned")
	}
	if promoted == nil || promoted.ResumeHint == "" {
		t.Fatalf("promoted = %+v, want a non-empty ResumeHint", promoted)
	}
	if !strings.Contains(promoteErr.Error(), "target moved") {
		t.Fatalf("err = %q, want a target-moved message", promoteErr)
	}
	if headOf(main) != advancedMain {
		t.Fatalf("main HEAD = %s, want unchanged %s (promote must not fall back to a real merge)", headOf(main), advancedMain)
	}
}

// Regression: withWorktreeLease refuses a second concurrent op on the same worktree and never runs its fn.
func TestWithWorktreeLeaseRefusesWhileHeldBeforeResolveRuns(t *testing.T) {
	t.Parallel()
	repo, main, wt := preflightFixtureSetup(t)
	plan := &LandPlan{WorktreePath: wt}
	mainBefore := headOf(main)

	if err := os.MkdirAll(operationsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = paths.TryFileLock(operationLeasePath(repo.Main, filepath.Base(wt)), 2*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	resolveWasCalled := false
	err := withWorktreeLease(repo, filepath.Base(wt), func() error {
		resolveWasCalled = true
		_, _, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
		return err
	})
	if err == nil {
		t.Fatal("withWorktreeLease: want an error while another operation holds the lease")
	}
	if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("err = %q, want it to report the lease conflict", err)
	}
	if resolveWasCalled {
		t.Fatal("ResolveWithAgent ran despite the lease being held — the lease refusal must happen before fn runs")
	}
	requireMergeHead(t, wt, false, "a conflict was materialized despite the lease refusal")
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

// An explicit Codex dispatch that fell back to Claude is reported as a mismatch, not accepted.
func TestResolveWithAgentExplicitCodexFallenBackToClaudeIsMismatch(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Second)
	resolveFixtureFakeAgent(t, func(ctx context.Context, agentName, prompt, workDir string, timeout time.Duration) (askEnvelope, error) {
		if agentName != "codex" {
			t.Fatalf("agentName = %q, want codex", agentName)
		}
		return askEnvelope{Fallback: "codex"}, nil
	})

	op, _, err := resolveWithAgent(context.Background(), repo, plan, "codex", nil)
	if err == nil {
		t.Fatal("ResolveWithAgent: want a mismatch error on fallback")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "fell back") {
		t.Fatalf("err = %q, want it to name the fallback mismatch", err)
	}
	requirePhase(t, "op", op.Phase, "failed")
	requireMergeHead(t, wt, true, "MERGE_HEAD was cleaned up on a fallback mismatch — no automatic cleanup is permitted")
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

func TestFinalizeAgentResolutionPropagatesScopeProbeError(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	ctx := context.Background()
	if _, err := materializeConflict(ctx, plan); err != nil {
		t.Fatal(err)
	}
	op := &Operation{Repo: repo.Main, Worktree: filepath.Base(wt), TargetRef: plan.TargetRef, TargetOID: plan.TargetOID,
		OriginalSourceOID: plan.SourceOID, ConflictPaths: []string{"f.txt"}, Phase: "resolving", AgentExitOK: true}
	before := headOf(wt)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	probeErr := errors.New("scope probe unavailable")
	original := runGit
	t.Cleanup(func() { runGit = original })
	runGit = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		if args[0] == "merge-tree" {
			return nil, probeErr
		}
		return original(ctx, dir, env, args...)
	}
	failed, result, err := finalizeAgentResolution(ctx, op, wt)
	if !errors.Is(err, probeErr) {
		t.Fatalf("scope probe error must fail resolution: op=%+v result=%+v err=%v", failed, result, err)
	}
	requireFlag(t, result == nil, true, "nil resolution result")
	requirePhase(t, "op", failed.Phase, "failed")
	if headOf(wt) != before {
		t.Fatal("scope probe failure must not create a resolution commit")
	}
	unmerged, err := unmergedPaths(ctx, wt)
	if err != nil || !slices.Contains(unmerged, "f.txt") {
		t.Fatalf("scope probe failure must leave the conflict unstaged: paths=%v err=%v", unmerged, err)
	}
	requireMergeHead(t, wt, true, "scope probe failure must preserve the in-progress merge")
}

// A failed save of the "resolved" checkpoint must surface, not report success with the phase only in memory.
func TestFinalizeAgentResolutionSurfacesCheckpointSaveFailure(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	ctx := context.Background()
	if _, err := materializeConflict(ctx, plan); err != nil {
		t.Fatal(err)
	}
	conflictPaths, err := unmergedPaths(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	op := &Operation{Repo: repo.Main, Worktree: filepath.Base(wt), TargetRef: plan.TargetRef, TargetOID: plan.TargetOID,
		OriginalSourceOID: plan.SourceOID, ConflictPaths: conflictPaths, Phase: "resolving", AgentExitOK: true}
	// AtomicWrite cannot rename over a directory
	if err := os.MkdirAll(operationPath(repo.Main, op.Worktree), 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err = finalizeAgentResolution(ctx, op, wt)
	if err == nil || !strings.Contains(err.Error(), "save resolved checkpoint") {
		t.Fatalf("finalizeAgentResolution err = %v, want a save resolved checkpoint error", err)
	}
}

func TestConflictMarkersClearHonorsCustomMarkerSize(t *testing.T) {
	repo := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("f.txt conflict-marker-size=9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		contents string
		wantPath string
	}{
		{"opening", "<<<<<<<<< HEAD\nours\n", "f.txt"},
		{"bare separator is a heading underline", "Title\n=========\ntext\n", ""},
		{"closing", "theirs\n>>>>>>>>> feature\n", "f.txt"},
		{"resolved", "resolved content\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			clean, badPath, err := conflictMarkersClear(context.Background(), repo, []string{"f.txt"})
			if err != nil {
				t.Fatal(err)
			}
			if clean != (tc.wantPath == "") {
				t.Fatalf("clean = %v for %q; want offending path %q", clean, tc.contents, tc.wantPath)
			}
			if badPath != tc.wantPath {
				t.Fatalf("badPath = %q, want %q", badPath, tc.wantPath)
			}
		})
	}
}

// A failed first SaveOperation must not leave a half-applied merge (MERGE_HEAD set) with nothing to resume.
func TestResolveWithAgentFreshSaveFailureAbortsMaterializedMerge(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (askEnvelope, error) {
		t.Fatal("the agent must not run when the operation record cannot be saved")
		return askEnvelope{}, nil
	})
	// a non-empty directory at the file path makes AtomicWrite fail
	if err := os.MkdirAll(filepath.Join(operationPath(repo.Main, filepath.Base(wt)), "x"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil || !strings.Contains(err.Error(), "merge aborted") {
		t.Fatalf("err = %v, want the save failure reported with the merge aborted", err)
	}
	requireMergeHead(t, wt, false, "abortMaterialized must run git merge --abort")
	landFixtureAssertMainUntouched(t, main, mainBefore)
}

func TestWithLandLeaseWaitsForHolderThenRuns(t *testing.T) {
	repo := testRepo(t)
	leasePath := operationLeasePath(repo.Main, "feature")
	if err := os.MkdirAll(filepath.Dir(leasePath), 0o755); err != nil {
		t.Fatal(err)
	}
	// a dead holder's leftover record does not block
	if err := os.WriteFile(leasePath+".who", []byte("999999\nlane-land hook\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	var got []string
	record := func(w string) error { got = append(got, w); return nil }
	if err := WithLandLease(context.Background(), repo, "feature", &log, record); err != nil || got[0] != "" || log.Len() != 0 {
		t.Fatalf("stale lease: err=%v got=%q log=%q, want an immediate run", err, got, log.String())
	}

	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = paths.TryFileLock(leasePath, 2*time.Second, func() error {
			_ = os.WriteFile(leasePath+".who", []byte("4242\nlane-land hook\n"), 0o600)
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	origSleep := sleepFn
	sleepFn = func(time.Duration) { close(release); <-done; sleepFn = origSleep }
	t.Cleanup(func() { sleepFn = origSleep })

	log.Reset()
	if err := WithLandLease(context.Background(), repo, "feature", &log, record); err != nil || got[1] != "lane-land hook" {
		t.Fatalf("held lease: err=%v got=%q, want a run after the wait", err, got)
	}
	if want := "land feature: waiting for the active land (pid 4242, lane-land hook)\n"; log.String() != want {
		t.Fatalf("log = %q, want %q", log.String(), want)
	}
}

func TestClosedByLandIgnoresHistoricalClose(t *testing.T) {
	closed := "- Status: closed\n\n## Opened\n\n- Base: x\n\n## Closed\n\n- Via: land\n"
	reopened := strings.Replace(closed, "closed", "open", 1) + "\n## Opened\n\n- Base: y\n"
	cleaned := strings.Replace(reopened, "open", "closed", 1) + "\n## Closed\n\n- Via: clean\n"
	for text, want := range map[string]bool{closed: true, reopened: false, cleaned: false} {
		if got := closedByLand(text); got != want {
			t.Errorf("closedByLand(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestResumeRejectsCommittedOutOfScopeEdits(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (askEnvelope, error) {
		regressionWriteFile(t, filepath.Join(wt, "f.txt"), "resolved\n")
		regressionWriteFile(t, filepath.Join(wt, "outside.txt"), "unauthorized\n")
		landFixtureRun(t, wt, "add", "f.txt", "outside.txt")
		landFixtureRun(t, wt, "commit", "--no-edit")
		return askEnvelope{}, nil
	})
	op, result, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err == nil || result != nil || !strings.Contains(err.Error(), "outside the conflicted paths") {
		t.Fatalf("fresh resolution must reject committed scope violation: result=%+v err=%v", result, err)
	}
	if !op.AgentExitOK {
		t.Fatal("fixture must preserve the successful agent exit for resume")
	}
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (askEnvelope, error) {
		t.Fatal("resume must not invoke the agent")
		return askEnvelope{}, nil
	})
	_, result, err = resolveWithAgent(context.Background(), repo, plan, "claude", op)
	if err == nil || result != nil || !strings.Contains(err.Error(), "outside the conflicted paths") {
		t.Fatalf("resume accepted the same committed scope violation: result=%+v err=%v", result, err)
	}
}

func TestCheckResolutionRejectsCommittedConflictMarkers(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	op := crashedOperation(t, repo, wt, plan)
	landFixtureRun(t, wt, "add", "f.txt")
	landFixtureRun(t, wt, "commit", "--no-edit")
	state, reason, err := checkResolution(context.Background(), op, wt)
	if err != nil || state != resolutionInvalid || !strings.Contains(reason, "conflict markers") {
		t.Fatalf("committed markers accepted: state=%v reason=%q err=%v", state, reason, err)
	}
}

func TestResolutionAllowsTrailingWhitespace(t *testing.T) {
	repo, _, wt, plan := resolveFixtureConflictSetup(t)
	resolveFixtureFakeAgent(t, func(context.Context, string, string, string, time.Duration) (askEnvelope, error) {
		regressionWriteFile(t, filepath.Join(wt, "f.txt"), "resolved with trailing space \n")
		if out, err := runGit(context.Background(), wt, nil, "diff", "--check"); err == nil {
			t.Fatalf("fixture must trigger git's whitespace warning: %q", out)
		}
		return askEnvelope{}, nil
	})
	op, result, err := resolveWithAgent(context.Background(), repo, plan, "claude", nil)
	if err != nil {
		t.Fatalf("whitespace without conflict markers must resolve: %v", err)
	}
	requirePhase(t, "operation", op.Phase, "resolved")
	requireIntegrate(t, result, headOf(wt))
	requireFileText(t, filepath.Join(wt, "f.txt"), "resolved with trailing space \n")
}

func regressionWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// a directory at the op path makes every save fail

// resolveFixtureOpEnv isolates paths.StateDir() (and so operationsDir) per test, like landFixtureLockEnv.
func resolveFixtureOpEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestSaveOperationThenLoadOperationRoundTrip(t *testing.T) {
	resolveFixtureOpEnv(t)
	started := time.Now().UTC().Truncate(time.Second)
	op := &Operation{
		ID:                "op-abc123",
		Repo:              "/repo/main",
		Worktree:          "feature",
		TargetRef:         "main",
		TargetOID:         "deadbeef",
		OriginalSourceOID: "cafef00d",
		ConflictPaths:     []string{"f.txt"},
		Phase:             "resolving",
		StartedAt:         started,
		Deadline:          started.Add(10 * time.Minute),
		Agent:             "claude",
		AgentExitOK:       true,
	}
	if err := SaveOperation(op); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	repo := &Repo{Main: "/repo/main"}
	got, err := loadOperation(repo, "feature")
	if err != nil {
		t.Fatalf("loadOperation: %v", err)
	}
	if !reflect.DeepEqual(got, op) {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, op)
	}
}

func TestSaveOperationOverwritesPriorStateForSameRepoWorktree(t *testing.T) {
	resolveFixtureOpEnv(t)
	repo := &Repo{Main: "/repo/main"}
	first := &Operation{Repo: "/repo/main", Worktree: "feature", Phase: "resolving", Agent: "claude"}
	if err := SaveOperation(first); err != nil {
		t.Fatalf("SaveOperation (first): %v", err)
	}
	second := &Operation{Repo: "/repo/main", Worktree: "feature", Phase: "resolved", Agent: "claude"}
	if err := SaveOperation(second); err != nil {
		t.Fatalf("SaveOperation (second): %v", err)
	}
	got, err := loadOperation(repo, "feature")
	if err != nil {
		t.Fatalf("loadOperation: %v", err)
	}
	if got.Phase != "resolved" {
		t.Fatalf("Phase = %q, want the overwritten %q (same repo+worktree key)", got.Phase, "resolved")
	}
}

// A missing operation file surfaces the plain os.ReadFile error so callers can use os.IsNotExist.
func TestLoadOperationMissingFileSurfacesNotExist(t *testing.T) {
	resolveFixtureOpEnv(t)
	repo := &Repo{Main: "/repo/main"}
	_, err := loadOperation(repo, "never-started")
	if err == nil {
		t.Fatal("loadOperation: want an error for a never-saved operation")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("loadOperation err = %v (%T), want os.IsNotExist true", err, err)
	}
}

// Garbage JSON at the operation path must yield an error, never a panic or a zero-value Operation.
func TestLoadOperationCorruptFileReturnsError(t *testing.T) {
	resolveFixtureOpEnv(t)
	repo := &Repo{Main: "/repo/main"}
	path := operationPath(repo.Main, "feature")
	if err := os.MkdirAll(operationsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadOperation(repo, "feature")
	if err == nil {
		t.Fatal("loadOperation: want an error for corrupt JSON")
	}
	if os.IsNotExist(err) {
		t.Fatalf("loadOperation err = %v, want a decode error, not not-exist", err)
	}
	if !strings.Contains(err.Error(), "load operation") {
		t.Fatalf("err = %q, want it to name the failing step", err)
	}
}

func TestOperationPathDistinguishesCollidingSlugs(t *testing.T) {
	a, b := operationPath("/r/x", "a-b"), operationPath("/r/x-a", "b")
	if a == b || operationPath("/r/x", "Fix") == operationPath("/r/x", "fix") {
		t.Errorf("operation paths collide: %s %s", a, b)
	}
}
