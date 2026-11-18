package worktree

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
)

func TestIntegrateTargetRebaseFailureStopsBeforePromote(t *testing.T) {
	repo, main, wt := laneMergeFixture(t)
	mergeFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")
	mergeFixtureWrite(t, main, "h.txt", "main file\n", "main adds h")
	mainBefore, wtBefore := headOf(main), headOf(wt)
	plan, err := Preflight(t.Context(), repo, "a", "")
	if err != nil {
		t.Fatal(err)
	}
	original := runGit
	t.Cleanup(func() { runGit = original })
	runGit = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		if slices.Contains(args, "rebase") {
			return nil, errors.New("pre-rebase rejected")
		}
		return original(ctx, dir, env, args...)
	}
	if _, err := integrateTarget(t.Context(), repo, plan); err == nil {
		t.Fatal("want rejected rebase")
	}
	if headOf(wt) != wtBefore || headOf(main) != mainBefore {
		t.Fatal("refs moved after rejected rebase")
	}
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

// Main advancing between integrateTarget and promote must stop promote with a retry hint, never a real merge.
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

// A main commit that is not an ancestor of the re-pinned worktree HEAD must make promote refuse rather than merge.
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

// Regression: a commit landing after integrateTarget must never be promoted; only the pinned OID is fast-forwarded.
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

// A preflightAfterResolution plan tolerates untracked tool residue; a plain Preflight plan refuses it.
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
	t.Setenv("HOME", t.TempDir())
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
	if err := acquire(time.Nanosecond, func() error { return nil }); err != nil {
		t.Fatalf("second immediate acquire: %v; lock was not released", err)
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

func laneMergeFixture(t *testing.T) (*Repo, string, string) {
	t.Helper()
	mergeFixtureLockEnv(t)
	main := paths.Canon(t.TempDir())
	mergeFixtureRun(t, main, "init", "-q", "-b", "main")
	mergeFixtureRun(t, main, "config", "user.name", "test")
	mergeFixtureRun(t, main, "config", "user.email", "test@example.invalid")
	mergeFixtureWrite(t, main, "f.txt", "base\n", "base")
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := Create(repo, "a", CreateOptions{Session: "session-a", Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(main)
	return repo, main, wt
}

func laneRequest(name string, out io.Writer) MergeRequest {
	return MergeRequest{Name: name, NoVerify: true, Out: out, Err: out,
		ShowPlan: func(*MergePlan) {}, ShowResolved: func(*MergeResult) {}}
}

// The gate sees trunk's file only on the rebased tree; the trailer ticks; lane b hears of the shared file.
func TestLaneMergeRebasesGatesTicksAndNotifies(t *testing.T) {
	repo, main, wt := laneMergeFixture(t)
	b, err := Create(repo, "b", CreateOptions{Session: "session-b", Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	mergeFixtureWrite(t, wt, "f.txt", "A\n", "feat: a\n\nTask: 2026-11-18-probe 1.1")
	mergeFixtureWrite(t, b, "f.txt", "B\n", "feat: b")
	mergeFixtureWrite(t, main, "trunk.txt", "trunk\n", "trunk moves")
	trunk := headOf(main)
	home := plans.Home(main)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(home, "2026-11-18-probe.md")
	if err := os.WriteFile(plan, []byte("- [ ] 1.1 Probe owns: f.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	req := laneRequest("a", &out)
	req.NoVerify, req.Verify = false, "test -f trunk.txt"
	req.AfterPromote = func(ctx context.Context, rng string) error {
		return plans.TickRange(ctx, main, "a", rng, "", nil, false, &out)
	}
	outcome, err := Merge(t.Context(), repo, req)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	requireContains(t, "output", out.String(), "rebased:", "verify: passed (flag)", "promoted:")
	parent, err := runGit(t.Context(), main, nil, "rev-parse", "HEAD~1")
	if err != nil || strings.TrimSpace(string(parent)) != trunk || outcome.Clean == nil || !outcome.Clean.Removed {
		t.Fatalf("want the lane commit fast-forwarded on top of trunk and the lane removed: %s %v %+v", parent, err, outcome.Clean)
	}
	if data, err := os.ReadFile(plan); err != nil || !strings.Contains(string(data), "[x] 1.1") {
		t.Fatalf("plan: %s, %v", data, err)
	}
	notices, err := DrainErr("session-b")
	if err != nil || len(notices) != 1 {
		t.Fatalf("notices: %+v %v", notices, err)
	}
	requireContains(t, "notice", notices[0].Text, "main moved to "+ShortOID(headOf(main)), "lane a merged", "overlaps yours in f.txt")
}

// A real conflict aborts the rebase: the lane keeps its HEAD and a clean tree, trunk stays, the session hears.
func TestLaneMergeConflictAbortsRebase(t *testing.T) {
	repo, main, wt := laneMergeFixture(t)
	mergeFixtureWrite(t, wt, "f.txt", "lane\n", "lane change")
	mergeFixtureWrite(t, main, "f.txt", "trunk\n", "trunk change")
	mainBefore, laneBefore := headOf(main), headOf(wt)
	_, err := Merge(t.Context(), repo, laneRequest("a", io.Discard))
	if !errors.Is(err, ErrRebaseConflict) {
		t.Fatalf("want rebase conflict, got %v", err)
	}
	requireContains(t, "conflict", err.Error(), "f.txt", "rebase aborted")
	if op, found, probeErr := inProgressOp(t.Context(), wt, ""); probeErr != nil || found {
		t.Fatalf("lane operation left: %s %v", op, probeErr)
	}
	if dirty, derr := isDirty(t.Context(), wt); derr != nil || dirty || headOf(wt) != laneBefore {
		t.Fatalf("lane changed: dirty=%v head %s want %s (%v)", dirty, headOf(wt), laneBefore, derr)
	}
	mergeFixtureAssertMainUntouched(t, main, mainBefore)
	notices, err := DrainErr("session-a")
	if err != nil || len(notices) != 1 || notices[0].Kind != "conflict" || !strings.Contains(notices[0].Text, "f.txt") {
		t.Fatalf("lane session notices = %+v %v, want one conflict notice naming f.txt", notices, err)
	}
}

func TestLaneMergeDryRunPreservesEveryRef(t *testing.T) {
	repo, main, wt := laneMergeFixture(t)
	mergeFixtureWrite(t, wt, "f.txt", "lane\n", "lane change")
	mergeFixtureWrite(t, main, "trunk.txt", "trunk\n", "trunk change")
	before, err := runGit(t.Context(), main, nil, "show-ref")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := PlanOrResolve(t.Context(), repo, laneRequest("a", &out), true); err != nil {
		t.Fatal(err)
	}
	after, err := runGit(t.Context(), main, nil, "show-ref")
	if err != nil || !bytes.Equal(before, after) || headOf(wt) == headOf(main) {
		t.Fatalf("refs changed: %s -> %s (%v)", before, after, err)
	}
	requireContains(t, "preview", out.String(), "would: rebase", "fast-forward main only", "remove the lane", "changed files: f.txt")
}

func TestLaneMergeDirtyOverlapRefusesBeforeRebase(t *testing.T) {
	repo, main, wt := laneMergeFixture(t)
	mergeFixtureWrite(t, wt, "f.txt", "lane\n", "lane change")
	before := headOf(wt)
	mainBefore := headOf(main)
	if err := os.WriteFile(filepath.Join(main, "f.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var plan *MergePlan
	req := laneRequest("a", io.Discard)
	req.ShowPlan = func(p *MergePlan) { plan = p }
	outcome, err := Merge(t.Context(), repo, req)
	if err != nil || outcome == nil || !outcome.Blocked {
		t.Fatalf("want refusal: %+v %v", outcome, err)
	}
	assertDirtyTargetIssue(t, plan, "f.txt")
	if headOf(wt) != before || headOf(main) != mainBefore {
		t.Fatal("ref moved before dirty-target refusal")
	}
	data, err := os.ReadFile(filepath.Join(main, "f.txt"))
	if err != nil || string(data) != "dirty\n" {
		t.Fatalf("edit lost: %s %v", data, err)
	}
}

// Trunk moving while the gate runs refuses the fast-forward and says re-run; the lane stays.
func TestLaneMergeTrunkMovedDuringGateRefuses(t *testing.T) {
	repo, main, wt := laneMergeFixture(t)
	mergeFixtureWrite(t, wt, "lane.txt", "lane\n", "lane change")
	var out bytes.Buffer
	req := laneRequest("a", &out)
	req.NoVerify, req.Verify = false, "git -C "+textutil.ShellQuote(main)+" commit -q --allow-empty -m moved"
	_, err := Merge(t.Context(), repo, req)
	if err == nil || !strings.Contains(err.Error(), "target moved") || !strings.Contains(out.String(), "re-run worktree merge") {
		t.Fatalf("want a target-moved refusal naming re-run: %v\n%s", err, out.String())
	}
	if headOf(main) == headOf(wt) {
		t.Fatal("trunk was fast-forwarded after it moved")
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("lane removed after a refused promote: %v", statErr)
	}
}

// Lanes up to date, without a session, or the merged one hear nothing; a behind lane gets one plain line.
func TestLaneMergeNonOverlappingNotice(t *testing.T) {
	repo, _, wt := laneMergeFixture(t)
	if _, err := Create(repo, "b", CreateOptions{Session: "session-b", Log: io.Discard}); err != nil {
		t.Fatal(err)
	}
	mergeFixtureWrite(t, wt, "lane.txt", "lane\n", "lane change")
	req := laneRequest("a", io.Discard)
	req.NoClean = true
	if _, err := Merge(t.Context(), repo, req); err != nil {
		t.Fatal(err)
	}
	notices, err := DrainErr("session-b")
	if err != nil || len(notices) != 1 {
		t.Fatalf("notices: %+v %v", notices, err)
	}
	requireContains(t, "notice", notices[0].Text, "main moved to", "lane a merged")
	if strings.Contains(notices[0].Text, "overlap") || strings.Contains(notices[0].Text, "\n") {
		t.Fatal("want one-line trunk moved notice")
	}
	if own, err := DrainErr("session-a"); err != nil || len(own) != 0 {
		t.Fatalf("merged lane notified: %+v %v", own, err)
	}
}

// --resolve-agent resolves an aborted rebase's conflict in the lane, then the merge lands.
func TestLaneMergeResolveAgentResolvesInLane(t *testing.T) {
	repo, main, wt := laneMergeFixture(t)
	mergeFixtureWrite(t, wt, "f.txt", "lane\n", "lane change")
	mergeFixtureWrite(t, main, "f.txt", "trunk\n", "trunk change")
	original := runResolveAgent
	t.Cleanup(func() { runResolveAgent = original })
	calls := 0
	runResolveAgent = func(ctx context.Context, name, prompt, dir string, timeout time.Duration) (askEnvelope, error) {
		calls++
		if name != "codex" || dir != wt || !strings.Contains(prompt, "f.txt") {
			t.Fatalf("dispatch: %s %s %s", name, dir, prompt)
		}
		return askEnvelope{}, os.WriteFile(filepath.Join(wt, "f.txt"), []byte("resolved\n"), 0o600)
	}
	req := laneRequest("a", io.Discard)
	req.ResolveAgent = "codex"
	if _, err := Merge(t.Context(), repo, req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(main, "f.txt"))
	if calls != 1 || err != nil || string(data) != "resolved\n" {
		t.Fatalf("calls %d; main f.txt = %q %v", calls, data, err)
	}
}

// CLI-created lanes each get their own inbox, and a lane kept by --no-clean stays open and still hears.
func TestLaneMergeCLILanesOwnInboxAndKeptLaneHears(t *testing.T) {
	repo, main, _ := laneMergeFixture(t)
	lane := map[string]string{}
	for _, n := range []string{"b", "c", "d"} {
		dir, err := Create(repo, n, CreateOptions{Session: cliSession, Log: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		lane[n] = dir
	}
	mergeFixtureWrite(t, lane["b"], "f.txt", "B\n", "feat: b")
	mergeFixtureWrite(t, lane["b"], "p.txt", "B\n", "feat: b p")
	for n, f := range map[string]string{"c": "f.txt", "d": "p.txt"} {
		if err := os.WriteFile(filepath.Join(lane[n], f), []byte(n+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	req := laneRequest("b", io.Discard)
	req.NoClean = true
	if _, err := Merge(t.Context(), repo, req); err != nil {
		t.Fatal(err)
	}
	for n, f := range map[string]string{"c": "f.txt", "d": "p.txt"} {
		notices, err := DrainFor("", func() string { return lane[n] })
		if err != nil || len(notices) != 1 || !strings.HasSuffix(notices[0].Text, "overlaps yours in "+f) {
			t.Fatalf("lane %s: want its own notice naming %s, got %+v %v", n, f, notices, err)
		}
	}
	if !strings.Contains(ledgerFile(t, main, "b"), "- Status: open\n") {
		t.Fatal("--no-clean closed the kept lane's ledger")
	}
	mergeFixtureRun(t, lane["c"], "checkout", "--", "f.txt")
	mergeFixtureWrite(t, lane["c"], "c.txt", "C\n", "feat: c")
	req = laneRequest("c", io.Discard)
	req.NoClean = true
	if _, err := Merge(t.Context(), repo, req); err != nil {
		t.Fatal(err)
	}
	kept, err := DrainFor("", func() string { return lane["b"] })
	if err != nil || len(kept) != 1 || !strings.Contains(kept[0].Text, "lane c merged") {
		t.Fatalf("kept lane b: want a notice for c's merge, got %+v %v", kept, err)
	}
}
