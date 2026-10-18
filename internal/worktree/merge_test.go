package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMergeFastForwardEndToEnd(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	sourceBefore := headOf(wt)

	plan, result, err := Merge(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", plan.Issues)
	}
	if result.Phase != "promote" {
		t.Fatalf("Phase = %q, want promote", result.Phase)
	}
	if headOf(main) != sourceBefore {
		t.Fatalf("main HEAD = %s, want fast-forwarded to %s", headOf(main), sourceBefore)
	}
	if result.AfterOID != headOf(main) {
		t.Fatalf("result.AfterOID = %s, want %s", result.AfterOID, headOf(main))
	}
}

func TestMergeDivergentCleanEndToEnd(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")
	mergeFixtureWrite(t, main, "h.txt", "main file\n", "main adds h")
	mainBefore := headOf(main)

	plan, result, err := Merge(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if plan.TargetOID != mainBefore {
		t.Fatalf("plan.TargetOID = %s, want pinned %s", plan.TargetOID, mainBefore)
	}
	if result.Phase != "promote" || result.AfterOID == "" {
		t.Fatalf("result = %+v, want a completed promote", result)
	}
	if headOf(main) != headOf(wt) {
		t.Fatalf("main HEAD %s != worktree HEAD %s after promote", headOf(main), headOf(wt))
	}
	// Both originals must be ancestors of the integrated commit.
	if ok, err := isAncestor(context.Background(), main, mainBefore, headOf(main)); err != nil || !ok {
		t.Fatalf("original main commit not an ancestor of the result: ok=%v err=%v", ok, err)
	}
}

func TestMergeRefusesOnDirtyWorktreeWithoutTouchingMain(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mainBefore := headOf(main)
	if err := os.WriteFile(filepath.Join(wt, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := Merge(context.Background(), repo, "feature", "")
	if err == nil {
		t.Fatal("Merge: want error for a dirty worktree")
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

func TestMergeRefusesOnDirtyTargetWithoutTouchingWorktree(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	wtBefore := headOf(wt)
	if err := os.WriteFile(filepath.Join(main, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := Merge(context.Background(), repo, "feature", "")
	if err == nil {
		t.Fatal("Merge: want error for a dirty target")
	}
	if headOf(wt) != wtBefore {
		t.Fatalf("worktree HEAD changed from %s to %s despite a blocked merge", wtBefore, headOf(wt))
	}
}

// TestIntegrateTargetHookFailureStopsBeforePromote installs a pre-merge-commit hook (shared via the
// repo's common git dir) that always fails, so `git merge` inside the worktree aborts mid-merge; the
// worktree is left with MERGE_HEAD, and main must never be touched since Promote is never reached.
func TestIntegrateTargetHookFailureStopsBeforePromote(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")
	mergeFixtureWrite(t, main, "h.txt", "main file\n", "main adds h")
	mainBefore := headOf(main)
	wtBefore := headOf(wt)

	commonDir, err := commonDirOf(context.Background(), main)
	if err != nil {
		t.Fatal(err)
	}
	hooksDir := filepath.Join(commonDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(hooksDir, "pre-merge-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Issues = %v, want none (hook failure happens inside git merge, not merge-tree)", plan.Issues)
	}

	if _, err := IntegrateTarget(context.Background(), repo, plan); err == nil {
		t.Fatal("IntegrateTarget: want error when the pre-merge-commit hook rejects")
	}
	if headOf(wt) != wtBefore {
		t.Fatalf("worktree HEAD advanced to %s despite the rejected merge commit (want %s)", headOf(wt), wtBefore)
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

// TestPromoteTargetMovedBetweenIntegrateAndPromote simulates another actor advancing main after
// IntegrateTarget ran but before Promote reacquires the lock and rechecks: Promote must stop with a
// retry hint, never fabricate a real conflicting merge.
func TestPromoteTargetMovedBetweenIntegrateAndPromote(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", plan.Issues)
	}
	if _, err := IntegrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}

	// Another actor advances main directly, after the plan pinned its OID.
	mergeFixtureWrite(t, main, "h.txt", "moved on\n", "main moves on")
	movedTo := headOf(main)

	result, err := Promote(context.Background(), repo, plan)
	if err == nil {
		t.Fatal("Promote: want error when the target moved since integration")
	}
	if result == nil || result.ResumeHint == "" {
		t.Fatalf("result = %+v, want a populated ResumeHint", result)
	}
	if headOf(main) != movedTo {
		t.Fatalf("main HEAD = %s, want untouched at %s", headOf(main), movedTo)
	}
}

// TestPromoteNotFastForwardAfterIntegration covers ancestry re-verification: if main is advanced with
// a commit that is not an ancestor of the (re-pinned) worktree HEAD, Promote must refuse the ff-only
// merge rather than fall back to a real merge commit.
func TestPromoteNotAncestorAfterTargetDiverges(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := IntegrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}

	// main advances past plan.TargetOID with a commit the integrated worktree HEAD never saw.
	mergeFixtureWrite(t, main, "f.txt", "main diverges\n", "main diverges")

	result, err := Promote(context.Background(), repo, plan)
	if err == nil {
		t.Fatal("Promote: want error — target moved, so promotion must stop regardless of ancestry")
	}
	if result == nil || result.ResumeHint == "" {
		t.Fatalf("result = %+v, want a populated ResumeHint", result)
	}
}

// TestPromoteRefusesCommitLandedAfterIntegrate is the regression test for Appendix A codex-review
// finding #2 (internal/worktree/merge.go:93): a commit landing in the worktree between IntegrateTarget
// and Promote (e.g. another actor, or the agent continuing to work) must never be silently promoted —
// Promote must only ever fast-forward main onto the OID IntegrateTarget actually produced and pinned.
func TestPromoteRefusesCommitLandedAfterIntegrate(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	integrated, err := IntegrateTarget(context.Background(), repo, plan)
	if err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}
	if plan.IntegratedOID != integrated.AfterOID {
		t.Fatalf("plan.IntegratedOID = %q, want it pinned to %q", plan.IntegratedOID, integrated.AfterOID)
	}

	// An unreviewed commit lands in the worktree after integration, before Promote ever runs.
	mergeFixtureWrite(t, wt, "unreviewed.txt", "slipped in after review\n", "unreviewed commit after integrate")
	unreviewed := headOf(wt)

	result, promoteErr := Promote(context.Background(), repo, plan)
	if promoteErr == nil {
		t.Fatal("Promote: want an error — the worktree moved past the verified integration since IntegrateTarget ran")
	}
	if result == nil || result.ResumeHint == "" {
		t.Fatalf("result = %+v, want a populated ResumeHint", result)
	}
	if headOf(main) == unreviewed {
		t.Fatal("main HEAD was fast-forwarded onto a commit that was never part of the reviewed integration")
	}
}

func TestPromoteLockHeldRefusesConcurrentMerge(t *testing.T) {
	repo, _, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := IntegrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("IntegrateTarget: %v", err)
	}

	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = Acquire(2*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	_, err = Promote(context.Background(), repo, plan)
	if err == nil {
		t.Fatal("Promote: want error while another holder has the merge lock")
	}
	if err != ErrMergeBusy {
		t.Fatalf("err = %v, want ErrMergeBusy", err)
	}
}

// TestIntegrateAndPromoteToleratePostResolutionResidue: a plan from PreflightAfterResolution lets untracked tool
// residue (a plugin cache) through integrate + promote, while a plain Preflight plan still refuses it.
func TestIntegrateAndPromoteToleratePostResolutionResidue(t *testing.T) {
	repo, main, wt := mergeFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	if err := os.MkdirAll(filepath.Join(wt, ".impeccable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".impeccable", "hook.cache.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	strict, err := Preflight(ctx, repo, "feature", "")
	if err != nil || len(strict.Issues) == 0 {
		t.Fatalf("plain Preflight must still flag untracked residue: issues=%v err=%v", strict.Issues, err)
	}
	plan, err := PreflightAfterResolution(ctx, repo, "feature", "")
	if err != nil || len(plan.Issues) != 0 {
		t.Fatalf("PreflightAfterResolution: issues=%v err=%v", plan.Issues, err)
	}
	if _, err := IntegrateTarget(ctx, repo, plan); err != nil {
		t.Fatalf("IntegrateTarget refused residue: %v", err)
	}
	if _, err := Promote(ctx, repo, plan); err != nil {
		t.Fatalf("Promote refused residue: %v", err)
	}
	if headOf(main) != headOf(wt) {
		t.Fatalf("main = %s, want promoted to %s", headOf(main), headOf(wt))
	}
}
