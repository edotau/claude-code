package worktree

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/platform/paths"
)

type drainTemplate struct {
	once           sync.Once
	main, wtA, wtB string
}

var drainTemplates = map[string]*drainTemplate{
	"b.txt":   {},
	"bad.txt": {},
}

func buildDrainTemplate(t *testing.T, bFile string) *drainTemplate {
	t.Helper()
	buildFixtureTemplate(t)
	dt := drainTemplates[bFile]
	dt.once.Do(func() {
		base := filepath.Join(fixture.dir, "drain-"+bFile)
		dt.main = filepath.Join(base, "repo")
		repo := &Repo{Main: dt.main, Root: filepath.Join(base, "worktrees", "repo")}
		dt.wtA = filepath.Join(repo.Root, "a")
		copyWorktreeAs(t, filepath.Join(fixture.dir, "repo"), filepath.Join(fixture.dir, "worktrees", "repo", "feature"),
			dt.main, dt.wtA, "feature", "a", "session-a", fixture.base)
		var err error
		dt.wtB, err = Create(repo, "b", CreateOptions{Session: "session-b", Log: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		landFixtureWrite(t, dt.wtA, "a.txt", "A\n", "feat: a")
		landFixtureWrite(t, dt.wtB, bFile, "B\n", "feat: b")
	})
	return dt
}

// drainFixture queues lanes a and b, each adding its own file; the gate appends a line to the returned counter.
func drainFixture(t *testing.T, bFile string) (*Repo, string, LandRequest) {
	t.Helper()
	dt := buildDrainTemplate(t, bFile)
	home := paths.Canon(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	main := paths.Canon(t.TempDir())
	repo := &Repo{Main: main, Root: filepath.Join(home, "worktrees", filepath.Base(main))}
	wtA, wtB := filepath.Join(repo.Root, "a"), filepath.Join(repo.Root, "b")
	copyFixtureTree(t, dt.main, main)
	copyFixtureTree(t, dt.wtA, wtA)
	copyFixtureTree(t, dt.wtB, wtB)
	rewireWorktree(t, main, wtA, "a", "session-a", fixture.base)
	rewireWorktree(t, main, wtB, "b", "session-b", fixture.base)
	for _, wt := range []string{wtA, wtB} {
		if err := EnqueueWithin(queueEditWait, QueuedLane{Lane: wt, Repo: main, Reason: QueueReady, Next: "land"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(main)
	count := filepath.Join(t.TempDir(), "gate-runs")
	var out bytes.Buffer
	req := laneRequest("", &out)
	req.NoVerify, req.Verify = false, "echo run >> "+count+"; test ! -f bad.txt"
	return repo, count, req
}

func gateRuns(t *testing.T, count string) int {
	t.Helper()
	raw, _ := os.ReadFile(count)
	return strings.Count(string(raw), "run")
}

func TestDrainQueueBatchesOneGateAndReportsEachLane(t *testing.T) {
	repo, count, req := drainFixture(t, "b.txt")
	res, err := DrainQueue(t.Context(), repo, req)
	if err != nil || len(res) != 2 {
		t.Fatalf("results %+v err %v", res, err)
	}
	for _, r := range res {
		if !r.Landed || !r.Batch || r.Err != nil {
			t.Fatalf("want every lane landed in the batch: %+v", res)
		}
	}
	if n := gateRuns(t, count); n != 1 {
		t.Fatalf("gate ran %d times, want one for the batch", n)
	}
	if left := Queued(repo.Main); len(left) != 0 {
		t.Fatalf("queue = %+v, want empty", left)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		requireFileContent(t, filepath.Join(repo.Main, f), strings.ToUpper(f[:1])+"\n")
	}
}

func TestDrainQueueBatchGateFailureIsolatesBadLane(t *testing.T) {
	repo, count, req := drainFixture(t, "bad.txt")
	res, err := DrainQueue(t.Context(), repo, req)
	if err != nil || len(res) != 2 {
		t.Fatalf("results %+v err %v", res, err)
	}
	byLane := map[string]DrainResult{res[0].Lane: res[0], res[1].Lane: res[1]}
	if a := byLane["a"]; !a.Landed || a.Err != nil {
		t.Fatalf("lane a = %+v, want landed", a)
	}
	if b := byLane["b"]; b.Landed || outcomeOf(b.Err) != OutcomeGateFailed {
		t.Fatalf("lane b = %+v, want a gate failure", b)
	}
	if n := gateRuns(t, count); n != 3 {
		t.Fatalf("gate ran %d times, want batch + a + b", n)
	}
	if left := Queued(repo.Main); len(left) != 1 || filepath.Base(left[0].Lane) != "b" {
		t.Fatalf("queue = %+v, want only b", left)
	}
}

func TestDrainQueueSecondDrainerLeavesQueueIntact(t *testing.T) {
	repo, count, req := drainFixture(t, "b.txt")
	lease := operationLeasePath(repo.Main, drainRunName)
	if err := os.MkdirAll(filepath.Dir(lease), 0o755); err != nil {
		t.Fatal(err)
	}
	ran, err := paths.TryFileLock(lease, 0, func() error {
		res, derr := DrainQueue(t.Context(), repo, req)
		if !errors.Is(derr, ErrDrainerBusy) || res != nil {
			t.Fatalf("second drainer = %+v, %v; want ErrDrainerBusy", res, derr)
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("hold lease: ran %v err %v", ran, err)
	}
	if n, runs := len(Queued(repo.Main)), gateRuns(t, count); n != 2 || runs != 0 {
		t.Fatalf("queue %d lanes, %d gate runs; want 2 and 0", n, runs)
	}
}

func laneDir(t *testing.T, repo *Repo, name string) string {
	t.Helper()
	dir, err := resolveWorktreeBounded(repo, name)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func queuedNames(repo *Repo) (names []string) {
	for _, q := range Queued(repo.Main) {
		names = append(names, filepath.Base(q.Lane))
	}
	return names
}

func TestDrainQueueLaneMovedAfterBatchStaysQueued(t *testing.T) {
	repo, _, req := drainFixture(t, "b.txt")
	a := laneDir(t, repo, "a")
	req.Verify = "git -C " + a + " commit -q --allow-empty -m late"
	res, err := DrainQueue(t.Context(), repo, req)
	if err != nil || len(res) != 2 {
		t.Fatalf("results %+v err %v", res, err)
	}
	for _, r := range res {
		switch r.Lane {
		case "a":
			if r.Landed || r.Left != "lane moved after the batch was built; left queued" {
				t.Fatalf("lane a = %+v, want left queued as moved", r)
			}
		case "b":
			if !r.Landed {
				t.Fatalf("lane b = %+v, want landed", r)
			}
		}
	}
	if got := queuedNames(repo); len(got) != 1 || got[0] != "a" {
		t.Fatalf("queue = %v, want only a", got)
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatalf("moved lane a was removed: %v", err)
	}
}

func TestDrainQueueSkipsLaneHeldByAnotherLand(t *testing.T) {
	repo, _, req := drainFixture(t, "b.txt")
	var res []DrainResult
	err := withWorktreeLease(repo, "a", func() (derr error) {
		res, derr = DrainQueue(t.Context(), repo, req)
		return derr
	})
	if err != nil {
		t.Fatal(err)
	}
	byLane := map[string]DrainResult{}
	for _, r := range res {
		byLane[r.Lane] = r
	}
	if a := byLane["a"]; a.Landed || a.Left == "" || !byLane["b"].Landed {
		t.Fatalf("results %+v, want a left queued and b landed", res)
	}
	if got := queuedNames(repo); len(got) != 1 || got[0] != "a" {
		t.Fatalf("queue = %v, want only a", got)
	}
	if _, err := os.Stat(filepath.Join(repo.Main, "a.txt")); err == nil {
		t.Fatal("leased lane a reached the live tree")
	}
}

// swapCollectorNames makes the drainer draw these collector names in order.
func swapCollectorNames(t *testing.T, names ...string) {
	t.Helper()
	old := collectorName
	t.Cleanup(func() { collectorName = old })
	collectorName = func() string {
		n := names[0]
		if len(names) > 1 {
			names = names[1:]
		}
		return n
	}
}

func TestDrainQueueNeverAdoptsExistingCollector(t *testing.T) {
	repo, _, req := drainFixture(t, "b.txt")
	pre, err := Create(repo, "land-batch-pre", CreateOptions{Session: "other", Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pre, "f.txt"), []byte("someone's edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	swapCollectorNames(t, "land-batch-pre", "land-batch-fresh")
	res, err := DrainQueue(t.Context(), repo, req)
	if err != nil || len(res) != 2 || !res[0].Landed || !res[1].Landed {
		t.Fatalf("results %+v err %v", res, err)
	}
	requireFileContent(t, filepath.Join(pre, "f.txt"), "someone's edit\n")
	if !branchExists(repo.Main, "worktree-land-batch-pre") {
		t.Fatal("the pre-existing collector's branch was deleted")
	}
	if _, err := os.Stat(laneDir(t, repo, "land-batch-fresh")); err == nil {
		t.Fatal("the run's own collector was not cleaned after landing")
	}
}

func TestDrainQueueKeepsDirtyCollectorOfItsOwnRun(t *testing.T) {
	repo, _, req := drainFixture(t, "bad.txt")
	req.Verify += "; touch junk.txt"
	swapCollectorNames(t, "land-batch-mine")
	if _, err := DrainQueue(t.Context(), repo, req); err != nil {
		t.Fatal(err)
	}
	requireFileContent(t, filepath.Join(laneDir(t, repo, "land-batch-mine"), "junk.txt"), "")
}

func TestDrainResultKeepsLandedWhenCleanupFails(t *testing.T) {
	repo, _, req := drainFixture(t, "b.txt")
	run := func(_ context.Context, _ *Repo, r LandRequest) (*landOutcome, error) {
		r.AfterLand("abc")
		return nil, errors.New("clean: boom")
	}
	r := landOne(t.Context(), repo, req, "a", run)
	if !r.Landed || r.Err != nil || !strings.Contains(r.Warning, "boom") || !strings.Contains(r.Line(), "landed, warning") {
		t.Fatalf("result %+v, want landed with a warning", r)
	}
}

// laneWithEvilMerge gives lane dir a merge commit that carries evil.txt, which git cherry never lists.
func laneWithEvilMerge(t *testing.T, dir string) {
	t.Helper()
	lane := strings.TrimSpace(gitOutputT(t, dir, "rev-parse", "--abbrev-ref", "HEAD"))
	landFixtureRun(t, dir, "branch", "side")
	landFixtureRun(t, dir, "checkout", "-q", "side")
	landFixtureWrite(t, dir, "s.txt", "S\n", "side")
	landFixtureRun(t, dir, "checkout", "-q", lane)
	landFixtureWrite(t, dir, "c.txt", "C\n", "lane again")
	landFixtureRun(t, dir, "merge", "--no-ff", "--no-commit", "side")
	if err := os.WriteFile(filepath.Join(dir, "evil.txt"), []byte("E\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	landFixtureRun(t, dir, "add", "evil.txt")
	landFixtureRun(t, dir, "commit", "-q", "-m", "merge side")
}

func gitOutputT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCmd(t.Context(), append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

func TestDrainQueueLeavesLaneWhoseMergeContentIsNotInBatch(t *testing.T) {
	repo, _, req := drainFixture(t, "b.txt")
	laneWithEvilMerge(t, laneDir(t, repo, "b"))
	res, err := DrainQueue(t.Context(), repo, req)
	if err != nil || len(res) != 2 {
		t.Fatalf("results %+v err %v", res, err)
	}
	for _, r := range res {
		switch r.Lane {
		case "a":
			if !r.Landed {
				t.Fatalf("lane a = %+v, want landed", r)
			}
		case "b":
			if r.Landed || !strings.Contains(r.Left, "content not in the batch (evil.txt)") || !strings.HasSuffix(r.Left, "left queued") {
				t.Fatalf("lane b = %+v, want left queued for evil.txt", r)
			}
		}
	}
	if got := queuedNames(repo); len(got) != 1 || got[0] != "b" {
		t.Fatalf("queue = %v, want only b", got)
	}
	if _, err := os.Stat(laneDir(t, repo, "b")); err != nil {
		t.Fatalf("lane b was removed: %v", err)
	}
}

func TestLaneContentInFlagsMissingAndDeletedPaths(t *testing.T) {
	_, main, wtA := laneLandFixture(t)
	landFixtureWrite(t, wtA, "a.txt", "A\n", "feat: add a")
	landFixtureRun(t, wtA, "rm", "-q", "f.txt")
	landFixtureRun(t, wtA, "commit", "-q", "-m", "feat: drop f")
	laneHead := headOf(wtA)
	if p, err := laneContentIn(t.Context(), main, laneHead, laneHead); p != "" || err != nil {
		t.Fatalf("same head = %q %v, want proven", p, err)
	}
	if p, _ := laneContentIn(t.Context(), main, laneHead, headOf(main)); p != "a.txt" {
		t.Fatalf("base as collector = %q, want a.txt", p)
	}
	addOnly := gitOutputT(t, wtA, "rev-parse", "HEAD~1")
	if p, _ := laneContentIn(t.Context(), main, laneHead, strings.TrimSpace(addOnly)); p != "f.txt" {
		t.Fatalf("collector keeping a deleted path = %q, want f.txt", p)
	}
	if p, err := laneContentIn(t.Context(), main, laneHead, "0000000000000000000000000000000000000000"); p == "" || err == nil {
		t.Fatalf("git error = %q %v, want different", p, err)
	}
}

func TestDrainQueueCollectorCreationIsExclusive(t *testing.T) {
	repo, _, _ := drainFixture(t, "b.txt")
	landFixtureRun(t, repo.Main, "branch", "worktree-land-batch-taken", "HEAD")
	taken := gitOutputT(t, repo.Main, "rev-parse", "worktree-land-batch-taken")
	swapCollectorNames(t, "land-batch-taken", "land-batch-free")
	b := &batchRun{}
	if err := b.create(repo); err != nil || b.collector != "land-batch-free" || b.branch != "worktree-land-batch-free" {
		t.Fatalf("create = %+v %v, want the free name", b, err)
	}
	if _, err := os.Stat(laneDir(t, repo, "land-batch-taken")); err == nil {
		t.Fatal("the taken name was checked out")
	}
	if got := gitOutputT(t, repo.Main, "rev-parse", "worktree-land-batch-taken"); got != taken {
		t.Fatal("the pre-existing branch moved")
	}
	if isTaken, err := addCollector(repo, "land-batch-taken", laneDir(t, repo, "land-batch-taken"), "worktree-land-batch-taken"); !isTaken || err != nil {
		t.Fatalf("addCollector on an existing branch = %v %v, want taken", isTaken, err)
	}
	b.discard(repo, io.Discard)
}

func TestDrainQueueDeletesCollectorBranchAfterBatch(t *testing.T) {
	repo, _, req := drainFixture(t, "b.txt")
	swapCollectorNames(t, "land-batch-ok")
	if res, err := DrainQueue(t.Context(), repo, req); err != nil || len(res) != 2 || !res[0].Landed || !res[1].Landed {
		t.Fatalf("results %+v err %v", res, err)
	}
	if branchExists(repo.Main, "worktree-land-batch-ok") {
		t.Fatal("the landed batch's collector branch was kept")
	}
}

func TestDrainQueueNamesCollectorLeftByFailedBatch(t *testing.T) {
	repo, _, req := drainFixture(t, "bad.txt")
	req.Verify += "; echo junk >> a.txt" // a tracked edit left by the gate blocks the promote
	swapCollectorNames(t, "land-batch-mine")
	if _, err := DrainQueue(t.Context(), repo, req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Err.(*bytes.Buffer).String(), "collector land-batch-mine left on disk") {
		t.Fatalf("the output does not name the collector left behind: %q", req.Err.(*bytes.Buffer).String())
	}
}

func TestDrainQueueCleanupFailureAfterApplyStillLandsAndDequeues(t *testing.T) {
	repo, _, req := drainFixture(t, "b.txt")
	old := batchLand
	t.Cleanup(func() { batchLand = old })
	batchLand = func(ctx context.Context, r *Repo, q LandRequest) (*landOutcome, error) {
		out, err := landLeased(ctx, r, q)
		return out, cmp.Or(err, errors.New("clean: boom"))
	}
	res, err := DrainQueue(t.Context(), repo, req)
	if err != nil || len(res) != 2 {
		t.Fatalf("results %+v err %v", res, err)
	}
	for _, r := range res {
		if !r.Landed || r.Err != nil || !strings.Contains(r.Warning, "boom") {
			t.Fatalf("lane = %+v, want landed with a warning", r)
		}
	}
	if left := Queued(repo.Main); len(left) != 0 {
		t.Fatalf("queue = %+v, want empty", left)
	}
	requireFileContent(t, filepath.Join(repo.Main, "b.txt"), "B\n")
}

func TestDrainPlanDryRunLeavesQueueFileUntouched(t *testing.T) {
	repo, _, req := drainFixture(t, "b.txt")
	if err := os.RemoveAll(laneDir(t, repo, "b")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(LandQueueFile())
	if err != nil {
		t.Fatal(err)
	}
	DrainPlan(t.Context(), repo, req)
	after, err := os.ReadFile(LandQueueFile())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("queue file changed by the dry run: %v\nbefore %s\nafter %s", err, before, after)
	}
}
