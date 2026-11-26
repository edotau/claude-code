package worktree

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

func TestIntegrateTargetRebaseFailureStopsBeforePromote(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")
	landFixtureWrite(t, main, "h.txt", "main file\n", "main adds h")
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
	if _, err := integrateTarget(t.Context(), plan); err == nil {
		t.Fatal("want rejected rebase")
	}
	if headOf(wt) != wtBefore || headOf(main) != mainBefore {
		t.Fatal("refs moved after rejected rebase")
	}
}

func TestPromoteLandsUncommittedSkipsSimulation(t *testing.T) {
	landFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "g.txt", "feature\n", "feature advances")
	ctx := context.Background()
	plan, err := Preflight(ctx, repo, "feature", "")
	if err != nil {
		t.Fatal(err)
	}
	integrated, err := integrateTarget(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "git.trace")
	t.Setenv("GIT_TRACE", trace)
	result, err := promoteOnly(ctx, repo, plan)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if result.AfterOID != integrated.AfterOID || headOf(main) != plan.TargetOID {
		t.Fatalf("promotion = %+v, HEAD = %s, want unmoved %s", result, headOf(main), plan.TargetOID)
	}
	requireFileContent(t, filepath.Join(main, "g.txt"), "feature\n")
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	requireContains(t, "promotion trace", string(data), "merge-base --is-ancestor")
	if strings.Contains(string(data), "merge-tree") || strings.Contains(string(data), "--ff-only") {
		t.Fatalf("proven fast-forward unnecessarily simulated a merge:\n%s", data)
	}
}

// Main advancing between integrateTarget and promote must stop promote with a retry hint, never a real merge.
func TestPromoteTargetMovedBetweenIntegrateAndPromote(t *testing.T) {
	landFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(plan.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", plan.Issues)
	}
	if _, err := integrateTarget(context.Background(), plan); err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}

	// Another actor advances main directly, after the plan pinned its OID.
	landFixtureWrite(t, main, "h.txt", "moved on\n", "main moves on")
	movedTo := headOf(main)

	result, err := promoteOnly(context.Background(), repo, plan)
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
	landFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := integrateTarget(context.Background(), plan); err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}

	// main advances past plan.TargetOID with a commit the integrated worktree HEAD never saw.
	landFixtureWrite(t, main, "f.txt", "main diverges\n", "main diverges")

	result, err := promoteOnly(context.Background(), repo, plan)
	if err == nil {
		t.Fatal("promote: want error — target moved, so promotion must stop regardless of ancestry")
	}
	if result == nil || result.ResumeHint == "" {
		t.Fatalf("result = %+v, want a populated ResumeHint", result)
	}
}

// Regression: a commit landing after integrateTarget must never be promoted; only the pinned OID lands.
func TestPromoteRefusesCommitLandedAfterIntegrate(t *testing.T) {
	landFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	integrated, err := integrateTarget(context.Background(), plan)
	if err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}
	if plan.IntegratedOID != integrated.AfterOID {
		t.Fatalf("plan.IntegratedOID = %q, want it pinned to %q", plan.IntegratedOID, integrated.AfterOID)
	}

	// An unreviewed commit lands in the worktree after integration, before promote ever runs.
	landFixtureWrite(t, wt, "unreviewed.txt", "slipped in after review\n", "unreviewed commit after integrate")
	unreviewed := headOf(wt)

	result, promoteErr := promoteOnly(context.Background(), repo, plan)
	if promoteErr == nil {
		t.Fatal("promote: want an error — the worktree moved past the verified integration since integrateTarget ran")
	}
	if result == nil || result.ResumeHint == "" {
		t.Fatalf("result = %+v, want a populated ResumeHint", result)
	}
	if _, err := os.Stat(filepath.Join(main, "unreviewed.txt")); err == nil || headOf(main) == unreviewed {
		t.Fatal("main received a commit that was never part of the reviewed integration")
	}
}

func TestPromoteLockHeldRefusesConcurrentLand(t *testing.T) {
	landFixtureLockEnv(t)
	repo, _, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "g.txt", "feature file\n", "feature adds g")

	plan, err := Preflight(context.Background(), repo, "feature", "")
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := integrateTarget(context.Background(), plan); err != nil {
		t.Fatalf("integrateTarget: %v", err)
	}

	origWait := landLockWait
	t.Cleanup(func() { landLockWait = origWait })
	landLockWait = 100 * time.Millisecond
	release := make(chan struct{})
	held := make(chan struct{})
	go func() {
		_ = acquire(2*time.Second, nil, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	_, err = promoteOnly(context.Background(), repo, plan)
	if err == nil {
		t.Fatal("promote: want error while another holder has the land lock")
	}
	if err != errLandBusy {
		t.Fatalf("err = %v, want errLandBusy", err)
	}
}

// A plan tolerates untracked tool residue, and integrate and promote carry on past it.
func TestIntegrateAndPromoteToleratePostResolutionResidue(t *testing.T) {
	landFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "g.txt", "feature only\n", "feature advances")
	if err := os.MkdirAll(filepath.Join(wt, ".impeccable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".impeccable", "hook.cache.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plan, err := Preflight(ctx, repo, "feature", "")
	if err != nil || len(plan.Issues) != 0 {
		t.Fatalf("Preflight: issues=%v err=%v", plan.Issues, err)
	}
	if _, err := integrateTarget(ctx, plan); err != nil {
		t.Fatalf("integrateTarget refused residue: %v", err)
	}
	if _, err := promoteOnly(ctx, repo, plan); err != nil {
		t.Fatalf("promote refused residue: %v", err)
	}
	requireFileContent(t, filepath.Join(main, "g.txt"), "feature only\n")
}

// dirtyLandCases are a lane change landing on an uncommitted live edit: no overlap, a clean merge, a conflict.
var dirtyLandCases = []struct {
	name, laneFile, laneBody, liveBody string
	want                               []string
	conflict                           bool
}{
	{"other file", "b.txt", "feature\n", "mine\n", []string{"mine\n"}, false},
	{"disjoint hunks merge", "f.txt", "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nTEN\n",
		"ONE\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n", []string{"ONE\n", "TEN\n"}, false},
	{"same lines keep both with markers", "f.txt", "LANE\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n",
		"LIVE\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n", []string{"<<<<<<<", "LIVE\n", "LANE\n", ">>>>>>>"}, true},
}

func TestLandDirtyTargetMergesBothSides(t *testing.T) {
	const base = "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	for _, tc := range dirtyLandCases {
		t.Run(tc.name, func(t *testing.T) {
			landFixtureLockEnv(t)
			repo, main, wt := preflightFixtureSetup(t)
			landFixtureWrite(t, main, "f.txt", base, "main grows f")
			landFixtureRun(t, wt, "merge", "-q", "--ff-only", headOf(main))
			landFixtureWrite(t, wt, tc.laneFile, tc.laneBody, "feature edits")
			before := headOf(main)
			writeLiveFile(t, main, "f.txt", tc.liveBody)
			var out bytes.Buffer
			outcome, err := Land(context.Background(), repo, LandRequest{
				Name: "feature", Verify: "true", NoClean: true, Out: &out, Err: io.Discard, ShowPlan: func(*LandPlan) {},
			})
			if err != nil || outcome == nil || outcome.Blocked {
				t.Fatalf("Land = %+v, %v; want it landed", outcome, err)
			}
			if headOf(main) != before {
				t.Fatal("main HEAD moved: a land stays uncommitted")
			}
			requireDirtyLandFiles(t, main, tc.laneFile, tc.laneBody, tc.want)
			if marked := strings.Contains(out.String(), "conflict markers left in"); marked != tc.conflict {
				t.Errorf("output %q: conflict note = %v, want %v", out.String(), marked, tc.conflict)
			}
		})
	}
}

func TestPromoteMergesIntoDirtyTargetAfterPlan(t *testing.T) {
	landFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "a.txt", "feature\n", "feature adds a")
	ctx := context.Background()
	plan, err := Preflight(ctx, repo, "feature", "")
	if err != nil || len(plan.Issues) != 0 {
		t.Fatalf("Preflight = %+v, %v", plan, err)
	}
	if _, err := integrateTarget(ctx, plan); err != nil {
		t.Fatal(err)
	}
	before := headOf(main)
	writeLiveFile(t, main, "a.txt", "uncommitted\n")
	res, err := promoteOnly(ctx, repo, plan)
	if err != nil || len(res.Conflicted) != 1 || res.Conflicted[0] != "a.txt" {
		t.Fatalf("promote = %+v, %v; want a.txt left with conflict markers", res, err)
	}
	got, _ := os.ReadFile(filepath.Join(main, "a.txt"))
	if !strings.Contains(string(got), "uncommitted\n") || !strings.Contains(string(got), "feature\n") || headOf(main) != before {
		t.Fatalf("a.txt = %q, head moved = %v; want both sides kept", got, headOf(main) != before)
	}
}

func writeLiveFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// requireDirtyLandFiles checks f.txt holds every want and, for a lane file other than f.txt, that it landed whole.
func requireDirtyLandFiles(t *testing.T, main, laneFile, laneBody string, want []string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(main, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if laneFile == "b.txt" {
		requireFileContent(t, filepath.Join(main, "b.txt"), laneBody)
	}
	for _, w := range want {
		if !strings.Contains(string(got), w) {
			t.Errorf("f.txt = %q, want it to hold %q", got, w)
		}
	}
}

// requireFileContent fails unless the live tree holds the lane's content uncommitted at path.
func requireFileContent(t *testing.T, path, want string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
	}
}

func landFixtureLockEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}

func TestAcquireRunsFnAndReleases(t *testing.T) {
	landFixtureLockEnv(t)
	ran := false
	if err := acquire(time.Second, nil, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run")
	}
	if err := acquire(time.Nanosecond, nil, func() error { return nil }); err != nil {
		t.Fatalf("second immediate acquire: %v; lock was not released", err)
	}
}

func TestAcquirePropagatesFnError(t *testing.T) {
	landFixtureLockEnv(t)
	sentinel := errFixtureSentinel{}
	err := acquire(time.Second, nil, func() error { return sentinel })
	if err != sentinel {
		t.Fatalf("err = %v, want the sentinel fn error", err)
	}
}

type errFixtureSentinel struct{}

func (errFixtureSentinel) Error() string { return "fixture sentinel error" }

func TestAcquireBusyReturnsErrLandBusyWithinWait(t *testing.T) {
	landFixtureLockEnv(t)
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = acquire(2*time.Second, nil, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	start := time.Now()
	wait := 100 * time.Millisecond
	err := acquire(wait, nil, func() error {
		t.Fatal("fn must not run while another holder has the lock")
		return nil
	})
	elapsed := time.Since(start)
	if err != errLandBusy {
		t.Fatalf("err = %v, want errLandBusy", err)
	}
	if elapsed < wait {
		t.Fatalf("acquire returned after %v, want it to have waited at least %v", elapsed, wait)
	}
}

func TestAfterLandCleanClosesLedgerOnce(t *testing.T) {
	landFixtureLockEnv(t)
	repo, main, wt := preflightFixtureSetup(t)
	landFixtureWrite(t, wt, "g.txt", "feature\n", "feature advances")
	wantHead := headOf(wt)
	outcome, err := Land(context.Background(), repo, LandRequest{
		Name: "feature", NoVerify: true, Out: io.Discard, Err: io.Discard,
		ShowPlan: func(*LandPlan) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Clean == nil || !outcome.Clean.Removed || outcome.Clean.BranchDeleted {
		t.Fatalf("clean = %+v, want the worktree removed and its branch kept", outcome.Clean)
	}
	text := ledgerFile(t, main, "feature")
	requireContains(t, "closed ledger", text, "- Via: land\n", "- Land: "+wantHead+"\n", "- Branch: kept\n")
	if strings.Count(text, "closed via land") != 1 || strings.Contains(text, "removed via") {
		t.Fatalf("land and cleanup must close the ledger exactly once:\n%s", text)
	}
}

func TestAcquireZeroWaitDefaultsToLandLockWait(t *testing.T) {
	landFixtureLockEnv(t)
	const short = 100 * time.Millisecond
	origWait := landLockWait
	t.Cleanup(func() { landLockWait = origWait })
	landLockWait = short
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = acquire(time.Second, nil, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	start := time.Now()
	err := acquire(0, nil, func() error { return nil })
	elapsed := time.Since(start)
	if err != errLandBusy {
		t.Fatalf("err = %v, want errLandBusy", err)
	}
	// wait<=0 must fall back to the default wait, not return immediately as a zero wait would imply.
	if elapsed < short-20*time.Millisecond {
		t.Fatalf("acquire(0, ...) returned after %v, want it to have used the default wait (%v)", elapsed, short)
	}
}

func laneLandFixture(t *testing.T) (*Repo, string, string) {
	t.Helper()
	landFixtureLockEnv(t)
	main := paths.Canon(t.TempDir())
	landFixtureRun(t, main, "init", "-q", "-b", "main")
	landFixtureRun(t, main, "config", "user.name", "test")
	landFixtureRun(t, main, "config", "user.email", "test@example.invalid")
	landFixtureWrite(t, main, "f.txt", "base\n", "base")
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

func laneRequest(name string, out io.Writer) LandRequest {
	return LandRequest{Name: name, NoVerify: true, Out: out, Err: out,
		ShowPlan: func(*LandPlan) {}, ShowResolved: func(*LandResult) {}}
}

// The gate sees trunk's file only on the rebased tree; main keeps its head and gets the lane's file uncommitted.
func TestLaneLandRebasesGatesAndLeavesMainUncommitted(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "f.txt", "A\n", "feat: a")
	landFixtureWrite(t, main, "trunk.txt", "trunk\n", "trunk moves")
	trunk := headOf(main)
	var out bytes.Buffer
	req := laneRequest("a", &out)
	req.NoVerify, req.Verify = false, "test -f trunk.txt"
	outcome, err := Land(t.Context(), repo, req)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	requireContains(t, "output", out.String(), "rebased:", "verify: passed (flag)", "landed a:")
	if headOf(main) != trunk || outcome.Clean == nil || !outcome.Clean.Removed {
		t.Fatalf("want main unmoved at %s and the lane removed: head %s %+v", trunk, headOf(main), outcome.Clean)
	}
	requireFileContent(t, filepath.Join(main, "f.txt"), "A\n")
}

// A real conflict aborts the rebase: the lane keeps its HEAD and a clean tree, trunk stays, the session hears.
func TestLaneLandConflictAbortsRebase(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "f.txt", "lane\n", "lane change")
	landFixtureWrite(t, main, "f.txt", "trunk\n", "trunk change")
	mainBefore, laneBefore := headOf(main), headOf(wt)
	_, err := Land(t.Context(), repo, laneRequest("a", io.Discard))
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
	landFixtureAssertMainUntouched(t, main, mainBefore)
	notices, err := DrainErr("session-a")
	if err != nil || len(notices) != 1 || notices[0].Kind != "conflict" || !strings.Contains(notices[0].Text, "f.txt") {
		t.Fatalf("lane session notices = %+v %v, want one conflict notice naming f.txt", notices, err)
	}
}

func TestLaneLandDryRunPreservesEveryRef(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "f.txt", "lane\n", "lane change")
	landFixtureWrite(t, main, "trunk.txt", "trunk\n", "trunk change")
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
	requireContains(t, "preview", out.String(), "would: rebase", "apply the lane's diff uncommitted", "remove the lane", "changed files: f.txt")
}

func TestLaneLandDirtyOverlapKeepsBothSides(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "f.txt", "lane\n", "lane change")
	if err := os.WriteFile(filepath.Join(main, "f.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outcome, err := Land(t.Context(), repo, laneRequest("a", io.Discard))
	if err != nil || outcome == nil || outcome.Blocked {
		t.Fatalf("want a land past the dirty path: %+v %v", outcome, err)
	}
	data, err := os.ReadFile(filepath.Join(main, "f.txt"))
	if err != nil || !strings.Contains(string(data), "dirty\n") || !strings.Contains(string(data), "lane\n") {
		t.Fatalf("f.txt = %q %v; want the edit and the lane's line both kept", data, err)
	}
}

// A lane's uncommitted tracked edits are committed before the land and land with it; a dry run only counts them.
func TestLaneLandCommitsLaneDirt(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "f.txt", "lane\n", "lane change")
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("lane, then more\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := headOf(wt)
	var out bytes.Buffer
	if _, err := PlanOrResolve(t.Context(), repo, laneRequest("a", &out), true); err != nil {
		t.Fatal(err)
	}
	requireContains(t, "dry run", out.String(), "would commit 1 uncommitted lane files")
	if headOf(wt) != before {
		t.Fatal("dry run committed in the lane")
	}
	outcome, err := Land(t.Context(), repo, laneRequest("a", io.Discard))
	if err != nil || outcome == nil || outcome.Blocked {
		t.Fatalf("Land = %+v, %v; want it landed past the lane's dirt", outcome, err)
	}
	requireFileContent(t, filepath.Join(main, "f.txt"), "lane, then more\n")
}

// Trunk moving while the gate runs refuses the fast-forward and says re-run; the lane stays.
func TestLaneLandTrunkMovedDuringGateRefuses(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "lane.txt", "lane\n", "lane change")
	var out bytes.Buffer
	req := laneRequest("a", &out)
	req.NoVerify, req.Verify = false, "git -C "+textutil.ShellQuote(main)+" commit -q --allow-empty -m moved"
	_, err := Land(t.Context(), repo, req)
	if err == nil || !strings.Contains(err.Error(), "target moved") || !strings.Contains(out.String(), "re-run worktree land") {
		t.Fatalf("want a target-moved refusal naming re-run: %v\n%s", err, out.String())
	}
	if headOf(main) == headOf(wt) {
		t.Fatal("main received the lane after it moved")
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("lane removed after a refused promote: %v", statErr)
	}
}

// --resolve-agent resolves an aborted rebase's conflict in the lane, then the merge lands.
func TestLaneLandResolveAgentResolvesInLane(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "f.txt", "lane\n", "lane change")
	landFixtureWrite(t, main, "f.txt", "trunk\n", "trunk change")
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
	if _, err := Land(t.Context(), repo, req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(main, "f.txt"))
	if calls != 1 || err != nil || string(data) != "resolved\n" {
		t.Fatalf("calls %d; main f.txt = %q %v", calls, data, err)
	}
}

func TestLaneLiveCheckRefusesLiveProcess(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := processesIn
	t.Cleanup(func() { processesIn = old })
	processesIn = func(string) ([]Proc, error) { return []Proc{{PID: 1 << 30, Comm: "sleep"}}, nil }
	if err := laneLiveCheck(t.TempDir()); err == nil || !strings.Contains(err.Error(), "leaf or job not finished") {
		t.Fatalf("want refusal, got %v", err)
	}
	processesIn = func(string) ([]Proc, error) { return nil, nil }
	if err := laneLiveCheck(t.TempDir()); err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func promoteOnly(ctx context.Context, repo *Repo, plan *LandPlan) (*LandResult, error) {
	res, _, err := promoteOrRebase(ctx, repo, plan, nil, false)
	return res, err
}

func TestTestTallySplitWrites(t *testing.T) {
	c := &testTally{w: io.Discard}
	for _, chunk := range []string{"ok  \tex/a\t0.1s\nFA", "IL\tex/b [build failed]\n--- FAIL: TestX\nFAIL\nok  \tex/c\t1s\n", "FAIL\tex/d\t1s"} {
		_, _ = c.Write([]byte(chunk))
	}
	c.flush()
	if want := []string{"ex/b", "ex/d"}; c.passed != 2 || !slices.Equal(c.failed, want) {
		t.Fatalf("passed %d failed %v, want 2 and %v", c.passed, c.failed, want)
	}
}

func TestAcquireNamesHolderWhileWaiting(t *testing.T) {
	landFixtureLockEnv(t)
	t.Setenv(LandWaitEnv, "holder-x")
	orig := landLockSlice
	t.Cleanup(func() { landLockSlice = orig })
	landLockSlice = 30 * time.Millisecond
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = acquire(2*time.Second, nil, func() error { close(held); <-release; return nil })
	}()
	<-held
	var by string
	err := acquire(150*time.Millisecond, func(_ time.Duration, _, who string) { by = who }, func() error { return nil })
	close(release)
	if err != errLandBusy || by != "holder-x" {
		t.Fatalf("err %v, holder %q, want errLandBusy and holder-x", err, by)
	}
}

// A second land waits in line with a named holder and gives up with the "another land is running" line.
func TestWithLandRunWaitsInLine(t *testing.T) {
	repo, _, _ := laneLandFixture(t)
	oldWait, oldPoll := maxLandWait, pollInterval
	t.Cleanup(func() { maxLandWait, pollInterval = oldWait, oldPoll })
	maxLandWait, pollInterval = 30*time.Millisecond, 5*time.Millisecond

	var out bytes.Buffer
	var waitErr error
	ran, err := paths.TryFileLock(operationLeasePath(repo.Main, landRunName), 0, func() error {
		waitErr = withLandRun(t.Context(), repo, "b", &out, func(time.Duration) error { return nil })
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("hold the lease: ran=%v err=%v", ran, err)
	}
	if waitErr == nil || !strings.Contains(waitErr.Error(), "another land is running") {
		t.Fatalf("want an another-land-is-running error, got %v", waitErr)
	}
	if !strings.Contains(out.String(), "waiting in line") {
		t.Fatalf("want the waiting line, got %q", out.String())
	}
	got := false
	if err := withLandRun(t.Context(), repo, "b", &out, func(time.Duration) error { got = true; return nil }); err != nil || !got {
		t.Fatalf("free lease: ran=%v err=%v", got, err)
	}
}

// A commit on main during the gate re-checks instead of failing, and both attempts are recorded.
func TestPromoteRevalidates(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "lane.txt", "lane\n", "lane change")
	flag := filepath.Join(t.TempDir(), "moved")
	var out bytes.Buffer
	req := laneRequest("a", &out)
	req.NoVerify = false
	req.Verify = "[ -e " + textutil.ShellQuote(flag) + " ] || { : > " + textutil.ShellQuote(flag) +
		"; git -C " + textutil.ShellQuote(main) + " commit -q --allow-empty -m moved; }"
	if _, err := Land(t.Context(), repo, req); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	requireContains(t, "output", out.String(), "rebase:     skipped", "rebased:", "re-check:", "landed a:")
	var outcomes []Outcome
	for _, a := range readAttempts(t) {
		outcomes = append(outcomes, a.Outcome)
	}
	if len(outcomes) != 2 || outcomes[0] != OutcomeTargetMoved || outcomes[1] != OutcomeLanded {
		t.Fatalf("attempt outcomes = %v, want [target_moved landed]", outcomes)
	}
}

// A re-check that fails after the target moved stops the land: nothing reaches main.
func TestPromoteFailedRecheckNeverLands(t *testing.T) {
	repo, main, wt := laneLandFixture(t)
	landFixtureWrite(t, wt, "lane.txt", "lane\n", "lane change")
	flag := filepath.Join(t.TempDir(), "moved")
	var out bytes.Buffer
	req := laneRequest("a", &out)
	req.NoVerify = false
	req.Verify = "if [ -e " + textutil.ShellQuote(flag) + " ]; then printf 'FAIL\\tex/z\\t0.1s\\n'; exit 1; fi; : > " + textutil.ShellQuote(flag) +
		"; git -C " + textutil.ShellQuote(main) + " commit -q --allow-empty -m moved"
	if _, err := Land(t.Context(), repo, req); err == nil || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("Land = %v, want verify failed\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(main, "lane.txt")); err == nil {
		t.Fatalf("a failed re-check still landed the lane\n%s", out.String())
	}
	if got := readAttempts(t); len(got) == 0 || strings.Join(got[len(got)-1].FailedPkgs, ",") != "ex/z" {
		t.Fatalf("last attempt must name the failed package ex/z: %+v", got)
	}
	if got, _ := ListFailures(FailuresDir()); len(got) != 1 || got[0].Kind != FailLaneGate {
		t.Fatalf("failed gate must broadcast one lane_gate failure: %+v", got)
	}
}

func readAttempts(t *testing.T) []LandAttempt {
	t.Helper()
	raw, err := os.ReadFile(LandAttemptsFile())
	if err != nil {
		t.Fatal(err)
	}
	return parseAttempts(raw)
}

// A gate that is not a Go package gate names its command and exit code in the broadcast summary.
func TestGateFailTextNamesCommandAndExit(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 3").Run()
	if got := gateFailText("false", err); got != "verify `false` exited 3" {
		t.Fatalf("gateFailText = %q", got)
	}
}

// A held gate gets a warm slot; with every slot busy it gives up after the wait and gates in the lane.
func TestHoldGate(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	top, sha := auditFixtureRepo(t)
	ctx := context.Background()
	oldWait, oldPoll := gateSlotWait, gateSlotPoll
	t.Cleanup(func() { gateSlotWait, gateSlotPoll = oldWait, oldPoll })
	gateSlotWait, gateSlotPoll = 0, time.Millisecond
	var held []*HeldGate
	for range slotCount {
		h := HoldGate(ctx, top, sha, "lane", &bytes.Buffer{})
		if h == nil || h.Where("lane-dir") == "lane-dir" {
			t.Fatalf("slot %d not held: %v", len(held), h)
		}
		held = append(held, h)
	}
	if h := HoldGate(ctx, top, sha, "lane", &bytes.Buffer{}); h != nil || h.Where("lane-dir") != "lane-dir" {
		t.Fatalf("all slots busy: got %v, want nil (lane)", h)
	}
	if err := held[0].Reset(ctx, sha); err != nil {
		t.Fatal(err)
	}
	for _, h := range held {
		h.Release()
	}
	if !usesGateSlot(fullGoGate, "go.mod") || usesGateSlot("make test", "Makefile") {
		t.Error("usesGateSlot picks the wrong gates")
	}
}
