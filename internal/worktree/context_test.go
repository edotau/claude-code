package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestContextAwareWorktreeLookupsReturnCancellation(t *testing.T) {
	repo, _, wt := mergeFixtureSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := resolveWorktree(ctx, repo, filepath.Base(wt)); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolveWorktree error = %v, want context.Canceled", err)
	}
	if _, err := ownsWorktree(ctx, repo, wt); !errors.Is(err, context.Canceled) {
		t.Fatalf("ownsWorktree error = %v, want context.Canceled", err)
	}
	if _, err := headOfContext(ctx, wt); !errors.Is(err, context.Canceled) {
		t.Fatalf("headOfContext error = %v, want context.Canceled", err)
	}
}

// TestPromoteCancelledContextNeverMovesMain covers Promote's own early ctx.Err() check: a context
// already cancelled when Promote is entered must return immediately without acquiring the merge lock
// or touching repo.Main at all — not even a failed fast-forward attempt.
func TestPromoteCancelledContextNeverMovesMain(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	mainBefore := headOf(main)

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := IntegrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Promote(ctx, repo, plan); err == nil {
		t.Fatal("Promote: want an error for an already-cancelled context")
	}
	if headOf(main) != mainBefore {
		t.Fatalf("main HEAD = %s, want unchanged %s — a cancelled ctx must never move it", headOf(main), mainBefore)
	}
}

// TestPromoteCancelledAfterChecksStillRefusesFastForward covers the second ctx.Err() check inside
// promoteLocked, right before the real `git merge --ff-only` runs: even once every recheck (branch,
// dirty, OIDs, ancestry, re-simulation) has already passed, a context cancelled in that window must
// still stop before the ff-only update, never racing a late cancellation into a real mutation.
func TestPromoteCancelledAfterChecksStillRefusesFastForward(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	mainBefore := headOf(main)

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := IntegrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	orig := runGit
	runGit = func(ctx2 context.Context, dir string, extraEnv []string, args ...string) ([]byte, error) {
		if slices.Contains(args, "--ff-only") {
			cancel()
		}
		return orig(ctx2, dir, extraEnv, args...)
	}
	defer func() { runGit = orig }()

	if _, err := Promote(ctx, repo, plan); err == nil {
		t.Fatal("Promote: want an error — ctx was cancelled just before the ff-only merge")
	}
	if headOf(main) != mainBefore {
		t.Fatalf("main HEAD = %s, want unchanged %s", headOf(main), mainBefore)
	}
}

// TestRunGitCancelledContextReturnsErrorNeverFalseClean covers the one seam over external git:
// runGit's production implementation wraps exec.CommandContext, so a cancelled ctx must surface as an
// error from the git call itself — never silently read as git's own "clean" exit code (0) by a caller
// like isAncestor/unrelatedHistories that distinguishes exit-code 1 from "everything else".
func TestRunGitCancelledContextReturnsErrorNeverFalseClean(t *testing.T) {
	_, main, _ := mergeFixtureSetup(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runGit(ctx, main, nil, "status", "--porcelain")
	if err == nil {
		t.Fatal("runGit: want an error for an already-cancelled context, not a successful empty result")
	}
}

// TestResolvePollingCancelledReturnsPromptlyPreservingStateThenResumeSucceeds covers --resume's poll
// loop (pollUntilResolved): a context cancelled mid-poll must return promptly (not wait out
// maxResolveWait), preserve the operation file and the worktree exactly as they were, and a later
// --resume (a fresh, uncancelled call) must still be able to pick the resolution up and succeed.
func TestResolvePollingCancelledReturnsPromptlyPreservingStateThenResumeSucceeds(t *testing.T) {
	repo, main, wt, plan := resolveFixtureConflictSetup(t)
	mainBefore := headOf(main)
	resolveFixtureFakeClock(t, time.Minute) // long enough that a real timeout would never fire first

	materialized, err := materializeConflict(context.Background(), plan)
	if err != nil {
		t.Fatalf("materializeConflict: %v", err)
	}
	if materialized.Phase != "conflict" {
		t.Fatalf("materialized.Phase = %q, want %q", materialized.Phase, "conflict")
	}

	op := &Operation{
		ID:                "op-cancel-poll",
		Repo:              repo.Main,
		Worktree:          filepath.Base(wt),
		TargetRef:         plan.TargetRef,
		TargetOID:         plan.TargetOID,
		OriginalSourceOID: plan.SourceOID,
		Phase:             "resolving",
		StartedAt:         nowFn(),
		Deadline:          nowFn().Add(time.Minute),
		Agent:             "claude",
		AgentExitOK:       true,
	}
	if err := SaveOperation(op); err != nil {
		t.Fatalf("SaveOperation: %v", err)
	}
	// The worktree is still genuinely mid-merge (MERGE_HEAD set, unmerged f.txt) — the poll loop must
	// never move past "pending" on its own; only the ctx cancellation below ends this call.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the very first loop iteration's ctx.Err() check must fire

	start := time.Now()
	gotOp, result, err := ResolveWithAgent(ctx, repo, plan, "claude", op)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ResolveWithAgent (resume, cancelled): want an error")
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil on cancellation", result)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("ResolveWithAgent took %s to return after cancellation, want it to stop promptly", elapsed)
	}
	if gotOp.Phase != "cancelled" {
		t.Fatalf("gotOp.Phase = %q, want %q", gotOp.Phase, "cancelled")
	}
	reloaded, loadErr := LoadOperation(repo, "feature")
	if loadErr != nil || reloaded.Phase != "cancelled" {
		t.Fatalf("reloaded = %+v, err=%v, want the preserved cancelled record", reloaded, loadErr)
	}
	mh, mhErr := hasMergeHead(context.Background(), wt)
	if mhErr != nil {
		t.Fatal(mhErr)
	}
	if !mh {
		t.Fatal("MERGE_HEAD was cleaned up after cancellation — no automatic cleanup is permitted")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)

	// A later --resume with a live (uncancelled) context must still be able to finish the job: resolve
	// the conflict for real now, matching what CheckResolution's own poll would have found.
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("resolved after cancel\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mergeFixtureRun(t, wt, "add", "f.txt")
	mergeFixtureRun(t, wt, "commit", "--no-edit")

	resumedOp, resumedResult, resumeErr := ResolveWithAgent(context.Background(), repo, plan, "claude", reloaded)
	if resumeErr != nil {
		t.Fatalf("ResolveWithAgent (resume after cancel): %v", resumeErr)
	}
	if resumedResult == nil || resumedResult.Phase != "integrate" {
		t.Fatalf("resumedResult = %+v, want a completed integrate", resumedResult)
	}
	if resumedOp.Phase != "resolved" {
		t.Fatalf("resumedOp.Phase = %q, want %q", resumedOp.Phase, "resolved")
	}
}

// TestCleanRefusesWhileWorktreeLeaseHeld covers Clean's own WithWorktreeLease hold (lifecycle.go): an
// ordinary clean must refuse outright, before ever rechecking the worktree's state, while another
// operation already holds the same worktree's lease — even one that would otherwise be eligible.
func TestCleanRefusesWhileWorktreeLeaseHeld(t *testing.T) {
	repo, main, _ := mergeFixtureSetup(t)
	mergeFixtureWrite(t, main, "h.txt", "main advances past the merged worktree\n", "main advances")

	if err := os.MkdirAll(operationsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = paths.TryFileLock(operationLeasePath(repo.Main, "feature"), 2*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	_, err := Clean(repo, "feature", false)
	if err == nil {
		t.Fatal("Clean: want an error while another operation holds the worktree lease")
	}
	if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("err = %q, want it to report the lease conflict", err)
	}
}
