package worktree

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIntegrateTargetHookFailureStopsBeforePromote installs a pre-merge-commit hook (shared via the
// repo's common git dir) that always fails, so `git merge` inside the worktree aborts mid-merge; the
// worktree is left with MERGE_HEAD, and main must never be touched since promote is never reached.
func TestIntegrateTargetHookFailureStopsBeforePromote(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
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

	if _, err := integrateTarget(context.Background(), repo, plan); err == nil {
		t.Fatal("integrateTarget: want error when the pre-merge-commit hook rejects")
	}
	if headOf(wt) != wtBefore {
		t.Fatalf("worktree HEAD advanced to %s despite the rejected merge commit (want %s)", headOf(wt), wtBefore)
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
}

func TestPromoteFastForwardSkipsSimulation(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature\n", "feature advances")
	ctx := context.Background()
	plan, err := Preflight(ctx, repo, "feature", "")
	if err != nil {
		t.Fatal(err)
	}
	integrated, err := integrateTarget(ctx, repo, plan)
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "git.trace")
	t.Setenv("GIT_TRACE", trace)
	result, err := promote(ctx, repo, plan)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if result.AfterOID != integrated.AfterOID || headOf(main) != integrated.AfterOID {
		t.Fatalf("promotion = %+v, HEAD = %s, want %s", result, headOf(main), integrated.AfterOID)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, "promotion trace", string(data), "merge-base --is-ancestor", "merge --ff-only")
	if strings.Contains(string(data), "merge-tree") {
		t.Fatalf("proven fast-forward unnecessarily simulated a merge:\n%s", data)
	}
}

// TestPromoteTargetMovedBetweenIntegrateAndPromote simulates another actor advancing main after
// integrateTarget ran but before promote reacquires the lock and rechecks: promote must stop with a
// retry hint, never fabricate a real conflicting merge.
func TestPromoteTargetMovedBetweenIntegrateAndPromote(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", plan.Issues)
	}
	if _, err := integrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}

	// Another actor advances main directly, after the plan pinned its OID.
	mergeFixtureWrite(t, main, "h.txt", "moved on\n", "main moves on")
	movedTo := headOf(main)

	result, err := promote(context.Background(), repo, plan)
	if err == nil {
		t.Fatal("promote: want error when the target moved since integration")
	}
	if result == nil || result.ResumeHint == "" {
		t.Fatalf("result = %+v, want a populated ResumeHint", result)
	}
	if headOf(main) != movedTo {
		t.Fatalf("main HEAD = %s, want untouched at %s", headOf(main), movedTo)
	}
}

// TestPromoteNotFastForwardAfterIntegration covers ancestry re-verification: if main is advanced with
// a commit that is not an ancestor of the (re-pinned) worktree HEAD, promote must refuse the ff-only
// merge rather than fall back to a real merge commit.
func TestPromoteNotAncestorAfterTargetDiverges(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := integrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}

	// main advances past plan.TargetOID with a commit the integrated worktree HEAD never saw.
	mergeFixtureWrite(t, main, "f.txt", "main diverges\n", "main diverges")

	result, err := promote(context.Background(), repo, plan)
	if err == nil {
		t.Fatal("promote: want error — target moved, so promotion must stop regardless of ancestry")
	}
	if result == nil || result.ResumeHint == "" {
		t.Fatalf("result = %+v, want a populated ResumeHint", result)
	}
}

// TestPromoteRefusesCommitLandedAfterIntegrate is the regression test for Appendix A codex-review
// finding #2 (internal/worktree/merge.go:93): a commit landing in the worktree between integrateTarget
// and promote (e.g. another actor, or the agent continuing to work) must never be silently promoted —
// promote must only ever fast-forward main onto the OID integrateTarget actually produced and pinned.
func TestPromoteRefusesCommitLandedAfterIntegrate(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	integrated, err := integrateTarget(context.Background(), repo, plan)
	if err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}
	if plan.IntegratedOID != integrated.AfterOID {
		t.Fatalf("plan.IntegratedOID = %q, want it pinned to %q", plan.IntegratedOID, integrated.AfterOID)
	}

	// An unreviewed commit lands in the worktree after integration, before promote ever runs.
	mergeFixtureWrite(t, wt, "unreviewed.txt", "slipped in after review\n", "unreviewed commit after integrate")
	unreviewed := headOf(wt)

	result, promoteErr := promote(context.Background(), repo, plan)
	if promoteErr == nil {
		t.Fatal("promote: want an error — the worktree moved past the verified integration since integrateTarget ran")
	}
	if result == nil || result.ResumeHint == "" {
		t.Fatalf("result = %+v, want a populated ResumeHint", result)
	}
	if headOf(main) == unreviewed {
		t.Fatal("main HEAD was fast-forwarded onto a commit that was never part of the reviewed integration")
	}
}

func TestPromoteLockHeldRefusesConcurrentMerge(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, _, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := integrateTarget(context.Background(), repo, plan); err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}

	origWait := mergeLockWait
	t.Cleanup(func() { mergeLockWait = origWait })
	mergeLockWait = 100 * time.Millisecond
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = acquire(2*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	_, err = promote(context.Background(), repo, plan)
	if err == nil {
		t.Fatal("promote: want error while another holder has the merge lock")
	}
	if err != errMergeBusy {
		t.Fatalf("err = %v, want errMergeBusy", err)
	}
}

// TestIntegrateAndPromoteToleratePostResolutionResidue: a plan from preflightAfterResolution lets untracked tool
// residue (a plugin cache) through integrate + promote, while a plain Preflight plan still refuses it.
func TestIntegrateAndPromoteToleratePostResolutionResidue(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
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
	plan, err := preflightAfterResolution(ctx, repo, "feature", "")
	if err != nil || len(plan.Issues) != 0 {
		t.Fatalf("preflightAfterResolution: issues=%v err=%v", plan.Issues, err)
	}
	if _, err := integrateTarget(ctx, repo, plan); err != nil {
		t.Fatalf("integrateTarget refused residue: %v", err)
	}
	if _, err := promote(ctx, repo, plan); err != nil {
		t.Fatalf("promote refused residue: %v", err)
	}
	if headOf(main) != headOf(wt) {
		t.Fatalf("main = %s, want promoted to %s", headOf(main), headOf(wt))
	}
}

func TestMergeDirtyTargetOverlap(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		name := "non-overlapping"
		if overlap {
			name = "overlapping"
		}
		t.Run(name, func(t *testing.T) {
			mergeFixtureLockEnv(t)
			repo, main, wt := preflightFixtureSetup(t)
			mergeFixtureWrite(t, main, "a.txt", "base\n", "main adds a")
			mergeFixtureWrite(t, wt, "b.txt", "feature\n", "feature adds b")
			if overlap {
				mergeFixtureWrite(t, wt, "a.txt", "feature\n", "feature adds a")
			}
			before := headOf(main)
			if err := os.WriteFile(filepath.Join(main, "a.txt"), []byte("uncommitted\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var plan *MergePlan
			outcome, err := Merge(context.Background(), repo, MergeRequest{
				Name: "feature", Verify: "true", NoClean: true, Out: io.Discard, Err: io.Discard,
				ShowPlan: func(p *MergePlan) { plan = p },
			})
			if err != nil || outcome == nil || outcome.Blocked != overlap {
				t.Fatalf("Merge = %+v, %v; want blocked=%v", outcome, err, overlap)
			}
			wantHead := headOf(wt)
			if overlap {
				assertDirtyTargetIssue(t, plan, "a.txt")
				wantHead = before
			}
			assertTargetEditIntact(t, main, wantHead)
		})
	}
}

func assertTargetEditIntact(t *testing.T, main, wantHead string) {
	t.Helper()
	if got := headOf(main); got != wantHead {
		t.Fatalf("main HEAD = %s, want %s", got, wantHead)
	}
	content, err := os.ReadFile(filepath.Join(main, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "uncommitted\n" {
		t.Fatalf("a.txt = %q, want uncommitted edit intact", content)
	}
}

func TestPromoteRechecksDirtyTargetOverlap(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "a.txt", "feature\n", "feature adds a")
	ctx := context.Background()
	plan, err := Preflight(ctx, repo, "feature", "")
	if err != nil || len(plan.Issues) != 0 {
		t.Fatalf("Preflight = %+v, %v", plan, err)
	}
	if _, err := integrateTarget(ctx, repo, plan); err != nil {
		t.Fatal(err)
	}
	before := headOf(main)
	if err := os.WriteFile(filepath.Join(main, "a.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := promote(ctx, repo, plan); err == nil || !strings.Contains(err.Error(), "dirty-target") || !strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("promote = %v, want dirty-target naming a.txt", err)
	}
	assertTargetEditIntact(t, main, before)
}

func mergeFixtureLockEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestAcquireRunsFnAndReleases(t *testing.T) {
	mergeFixtureLockEnv(t)
	ran := false
	if err := acquire(time.Second, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run")
	}
	// The lock must be released: a second acquire must succeed promptly, not wait out mergeLockWait.
	start := time.Now()
	if err := acquire(time.Second, func() error { return nil }); err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("second acquire took %v, want near-instant (lock should have been released)", elapsed)
	}
}

func TestAcquirePropagatesFnError(t *testing.T) {
	mergeFixtureLockEnv(t)
	sentinel := errFixtureSentinel{}
	err := acquire(time.Second, func() error { return sentinel })
	if err != sentinel {
		t.Fatalf("err = %v, want the sentinel fn error", err)
	}
}

type errFixtureSentinel struct{}

func (errFixtureSentinel) Error() string { return "fixture sentinel error" }

func TestAcquireBusyReturnsErrMergeBusyWithinWait(t *testing.T) {
	mergeFixtureLockEnv(t)
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = acquire(2*time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	start := time.Now()
	wait := 100 * time.Millisecond
	err := acquire(wait, func() error {
		t.Fatal("fn must not run while another holder has the lock")
		return nil
	})
	elapsed := time.Since(start)
	if err != errMergeBusy {
		t.Fatalf("err = %v, want errMergeBusy", err)
	}
	if elapsed < wait {
		t.Fatalf("acquire returned after %v, want it to have waited at least %v", elapsed, wait)
	}
}

func TestAfterPromoteCleanClosesLedgerOnce(t *testing.T) {
	mergeFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature\n", "feature advances")
	wantHead := headOf(wt)
	outcome, err := Merge(context.Background(), repo, MergeRequest{
		Name: "feature", NoVerify: true, Out: io.Discard, Err: io.Discard,
		ShowPlan: func(*MergePlan) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Clean == nil || !outcome.Clean.Removed || !outcome.Clean.BranchDeleted {
		t.Fatalf("clean = %+v, want worktree and branch removed", outcome.Clean)
	}
	text := ledgerFile(t, main, "feature")
	requireContains(t, "closed ledger", text, "- Via: merge\n", "- Merge: "+wantHead+"\n", "- Branch: deleted\n")
	if strings.Count(text, "closed via merge") != 1 || strings.Contains(text, "removed via") {
		t.Fatalf("merge and cleanup must close the ledger exactly once:\n%s", text)
	}
}

func TestAcquireZeroWaitDefaultsToMergeLockWait(t *testing.T) {
	mergeFixtureLockEnv(t)
	const short = 100 * time.Millisecond
	origWait := mergeLockWait
	t.Cleanup(func() { mergeLockWait = origWait })
	mergeLockWait = short
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = acquire(time.Second, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	start := time.Now()
	err := acquire(0, func() error { return nil })
	elapsed := time.Since(start)
	if err != errMergeBusy {
		t.Fatalf("err = %v, want errMergeBusy", err)
	}
	// wait<=0 must fall back to the default wait, not return immediately as a zero wait would imply.
	if elapsed < short-20*time.Millisecond {
		t.Fatalf("acquire(0, ...) returned after %v, want it to have used the default wait (%v)", elapsed, short)
	}
}
