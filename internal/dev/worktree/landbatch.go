package worktree

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// QueueReady is the queue reason of a lane the hook judged finished and clean; only such lanes are drained.
const QueueReady = "finished and clean, ready to land"

// ErrDrainerBusy marks a second drainer for one checkout: it changes nothing and leaves its lanes queued.
var ErrDrainerBusy = errors.New("land drain: another drainer is running for this checkout")

// drainRunName keys the one drainer lease per checkout.
const drainRunName = "(land-drain)"

// DrainResult is one queued lane's outcome: landed, left queued (Left says why) or failed (Err).
type DrainResult struct {
	Lane   string
	Landed bool
	Batch  bool // landed through the one batch gate
	Left   string
	// Warning is a cleanup error after the lane landed and was dequeued.
	Warning string
	Err     error
}

// Line is the one-line result a drainer prints.
func (r DrainResult) Line() string {
	switch {
	case r.Err != nil:
		return fmt.Sprintf("drain %s: failed, left queued: %v", r.Lane, r.Err)
	case r.Landed && r.Warning != "":
		return fmt.Sprintf("drain %s: landed, warning: %s", r.Lane, r.Warning)
	case r.Landed && r.Batch:
		return fmt.Sprintf("drain %s: landed (batch gate)", r.Lane)
	case r.Landed:
		return fmt.Sprintf("drain %s: landed", r.Lane)
	}
	return fmt.Sprintf("drain %s: left queued: %s", r.Lane, r.Left)
}

// DrainQueue is the one drainer of a checkout: ready lanes batch under one gate, a failed gate falls back lane by lane.
func DrainQueue(ctx context.Context, repo *Repo, req LandRequest) (res []DrainResult, err error) {
	lease := operationLeasePath(repo.Main, drainRunName)
	if err := os.MkdirAll(filepath.Dir(lease), 0o755); err != nil {
		return nil, err
	}
	ran, err := paths.TryFileLock(lease, 0, func() error {
		_ = paths.AtomicWrite(lease+".who", []byte(strconv.Itoa(os.Getpid())+"\n"+cmp.Or(os.Getenv(LandWaitEnv), "manual drain")+"\n"), 0o600)
		res = drain(ctx, repo, req)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !ran {
		pid, by := leaseHolder(lease + ".who")
		return nil, fmt.Errorf("%w (pid %s, %s)", ErrDrainerBusy, pid, by)
	}
	return res, nil
}

func readyLanes(repo *Repo, readOnly bool) (names []string) {
	queued := Queued
	if readOnly {
		queued = QueuedReadOnly
	}
	for _, q := range queued(repo.Main) {
		if q.Reason == QueueReady {
			names = append(names, filepath.Base(q.Lane))
		}
	}
	return names
}

// DrainPlan is what DrainQueue would do, one line each; it changes nothing.
func DrainPlan(ctx context.Context, repo *Repo, req LandRequest) []string {
	names := readyLanes(repo, true)
	if len(names) == 0 {
		return []string{"drain: no ready lanes queued"}
	}
	gate, source, err := chooseVerifyGate(repo.Main, req.Verify, req.NoVerify)
	gateLine := "verify: skipped (--no-verify)"
	switch {
	case err != nil:
		gateLine = "verify: " + err.Error()
	case gate != "":
		gateLine = fmt.Sprintf("verify: %s (%s)", gate, source)
	}
	lines := []string{fmt.Sprintf("drain: dry run, %d ready lanes: %s", len(names), strings.Join(names, ", "))}
	var usable []string
	for _, n := range names {
		creq := CollapseRequest{Lanes: []string{n}, Keep: true}
		if _, err := collapseLanes(ctx, repo, creq); err != nil {
			lines = append(lines, fmt.Sprintf("drain %s: would be left queued: %v", n, err))
		} else {
			usable = append(usable, n)
		}
	}
	switch {
	case len(names) == 1:
		lines = append(lines, fmt.Sprintf("drain: would land %s alone", names[0]))
	case len(usable) > 0:
		lines = append(lines, fmt.Sprintf("drain: would batch %s under one gate", strings.Join(usable, ", ")))
	}
	return append(lines, gateLine)
}

func drain(ctx context.Context, repo *Repo, req LandRequest) []DrainResult {
	names := readyLanes(repo, false)
	switch len(names) {
	case 0:
		return nil
	case 1:
		return []DrainResult{landOne(ctx, repo, req, names[0], landLeased)}
	}
	return landBatch(ctx, repo, req, names)
}

// landOne is one `worktree land <name>` through run; it neither prints nor exits.
func landOne(ctx context.Context, repo *Repo, req LandRequest, name string, run landFunc) DrainResult {
	landed := false
	req.Name, req.AfterLand = name, func(string) { landed = true }
	out, err := run(ctx, repo, req)
	switch {
	case err != nil && landed: // applied and dequeued; only the cleanup failed
		return DrainResult{Lane: name, Landed: true, Warning: err.Error()}
	case err != nil:
		return DrainResult{Lane: name, Err: err}
	case out.Blocked:
		return DrainResult{Lane: name, Left: "blocked by its plan (shown above)"}
	}
	return DrainResult{Lane: name, Landed: true}
}

type landFunc func(context.Context, *Repo, LandRequest) (*landOutcome, error)

// batchLand lands the collector; tests swap it.
var batchLand landFunc = landLeased

// landLeased lands req.Name under its lease, as `worktree land <name>` does.
func landLeased(ctx context.Context, repo *Repo, req LandRequest) (out *landOutcome, err error) {
	lerr := WithLandLease(ctx, repo, req.Name, req.Out, func(string) error {
		out, err = Land(ctx, repo, req)
		return nil
	})
	return out, cmp.Or(lerr, err)
}

// holdLanes takes each lane's worktree lease (one try); a busy lane gets a left-queued result and is skipped.
func holdLanes(repo *Repo, names, held []string, res *[]DrainResult, fn func(held []string)) {
	if len(names) == 0 {
		fn(held)
		return
	}
	ran := false
	err := withWorktreeLease(repo, names[0], func() error {
		ran = true
		holdLanes(repo, names[1:], append(held, names[0]), res, fn)
		return nil
	})
	if !ran {
		*res = append(*res, DrainResult{Lane: names[0], Left: cmp.Or(errText(err, ""), "lease not taken")})
		holdLanes(repo, names[1:], held, res, fn)
	}
}

// batchRun is one batch: the collector this run created and each picked lane's head when it was copied.
type batchRun struct {
	collector, dir, branch string
	heads                  map[string]string
}

// landBatch holds every source lane's lease, picks the lanes into one collector, gates and lands it once.
func landBatch(ctx context.Context, repo *Repo, req LandRequest, names []string) (res []DrainResult) {
	holdLanes(repo, names, nil, &res, func(held []string) {
		if len(held) > 0 {
			res = append(res, landHeldBatch(ctx, repo, req, held)...)
		}
	})
	return res
}

func landHeldBatch(ctx context.Context, repo *Repo, req LandRequest, names []string) []DrainResult {
	b := &batchRun{heads: map[string]string{}}
	res, lanes, picked := b.collect(ctx, repo, req, names)
	if len(picked) == 0 {
		return res
	}
	var head string
	breq := req
	breq.Name, breq.NoClean, breq.AfterLand = b.collector, false, func(h string) { head = h }
	fmt.Fprintf(req.Out, "drain: %d lanes in batch %s, one gate\n", len(picked), b.collector)
	out, err := batchLand(ctx, repo, breq)
	// head is set once the apply succeeded; a later cleanup error then leaves the work landed in the live tree
	if head != "" || (err == nil && !out.Blocked) {
		for _, l := range lanes {
			if slices.Contains(picked, l.name) {
				r := closeBatchLane(ctx, repo, req, l, b, head)
				if r.Landed && err != nil && r.Warning == "" {
					r.Warning = fmt.Sprintf("batch %s cleanup: %v", b.collector, err)
				}
				res = append(res, r)
			}
		}
		b.deleteBranch(repo, req.Err)
		return res
	}
	b.discard(repo, req.Err)
	if err != nil && outcomeOf(err) == OutcomeGateFailed {
		fmt.Fprintf(req.Out, "drain: batch gate failed; landing the %d lanes one by one\n", len(picked))
		for _, n := range picked {
			res = append(res, landOne(ctx, repo, req, n, Land))
		}
		return res
	}
	for _, n := range picked {
		res = append(res, DrainResult{Lane: n, Err: cmp.Or(err, errors.New("batch blocked by its plan (shown above)"))})
	}
	return res
}

// collect picks the usable lanes into a new collector; res holds a left-queued result per lane it could not pick.
func (b *batchRun) collect(ctx context.Context, repo *Repo, req LandRequest, names []string) (res []DrainResult, lanes []collapseLane, picked []string) {
	creq := CollapseRequest{Out: req.Out, Keep: true}
	for _, n := range names {
		head := ""
		if dir, err := resolveWorktree(ctx, repo, n); err == nil {
			head = headOf(dir)
		}
		creq.Lanes = []string{n}
		if l, err := collapseLanes(ctx, repo, creq); err != nil {
			res = append(res, DrainResult{Lane: n, Left: err.Error()})
		} else {
			lanes = append(lanes, l...)
			b.heads[n] = head
		}
	}
	if len(lanes) == 0 {
		return res, nil, nil
	}
	var dropped []string
	err := b.create(repo)
	if err == nil {
		creq.Collector = b.collector
		picked, dropped, err = pickBatch(ctx, repo, b.dir, creq, lanes)
	}
	for _, l := range lanes {
		if !slices.Contains(picked, l.name) {
			res = append(res, DrainResult{Lane: l.name, Left: cmp.Or(droppedWhy(dropped, l.name), errText(err, "not picked"))})
		}
	}
	if len(picked) == 0 {
		b.discard(repo, req.Err)
	}
	return res, lanes, picked
}

// collectorName is a fresh collector name: one-second time plus a random suffix; tests swap it.
var collectorName = func() string {
	return "land-batch-" + strconv.FormatInt(time.Now().Unix(), 36) + "-" + rand.Text()[:8]
}

// create makes a collector no one else has: `worktree add -b` fails on an existing branch, so that name is skipped.
func (b *batchRun) create(repo *Repo) error {
	for range 8 {
		name := collectorName()
		dest, err := resolveWorktreeBounded(repo, name)
		if err != nil {
			return err
		}
		branch, err := createBranchName(name, "")
		if err != nil {
			return err
		}
		if _, err := os.Stat(dest); err == nil {
			continue
		}
		taken, err := addCollector(repo, name, dest, branch)
		if taken {
			continue
		}
		if err != nil {
			return err
		}
		b.collector, b.dir, b.branch = name, dest, branch
		return nil
	}
	return errors.New("no unused collector name after 8 tries")
}

// addCollector runs the exclusive `git worktree add -b`; taken reports the branch or directory already existed.
func addCollector(repo *Repo, name, dest, branch string) (taken bool, err error) {
	if err := os.MkdirAll(repo.Root, 0o755); err != nil {
		return false, err
	}
	baseRef := createBase(repo.Main, io.Discard)
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	base, baseErr := gitCmd(ctx, "-C", repo.Main, "rev-parse", "--verify", baseRef+"^{commit}").Output()
	if baseErr != nil {
		return false, fmt.Errorf("resolve %s: %w", baseRef, baseErr)
	}
	out, err := gitCmd(ctx, "-C", repo.Main, "worktree", "add", "--no-track", "-b", branch, dest, baseRef).CombinedOutput()
	if err != nil {
		if branchExists(repo.Main, branch) || paths.IsDir(dest) {
			return true, nil // another process holds the name; ours failed before creating anything it owns
		}
		return false, fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	seedWorktree(repo.Main, dest, io.Discard)
	sha := strings.TrimSpace(string(base))
	recordForkPoint(repo.Main, branch, base, nil, io.Discard)
	_ = openLedger(repo.Main, name, ledgerInfo{Path: dest, Branch: branch, Base: sha, BaseRef: baseRef, Session: "cli"})
	return false, nil
}

func errText(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// pickBatch cherry-picks lanes into the collector dir under its lease; a conflicting lane is dropped.
func pickBatch(ctx context.Context, repo *Repo, dir string, creq CollapseRequest, lanes []collapseLane) (picked, dropped []string, err error) {
	err = withWorktreeLease(repo, creq.Collector, func() error {
		if err := collapseReady(ctx, dir); err != nil {
			return err
		}
		var perr error
		picked, dropped, perr = collapsePicks(ctx, repo, dir, lanes, creq)
		return perr
	})
	return picked, dropped, err
}

// droppedWhy is the reason in lane's "name (reason)" entry of dropped.
func droppedWhy(dropped []string, lane string) string {
	for _, d := range dropped {
		if why, ok := strings.CutPrefix(d, lane+" ("); ok {
			return "dropped from the batch: " + strings.TrimSuffix(why, ")")
		}
	}
	return ""
}

// closeBatchLane dequeues and removes a landed lane like afterLand, unless its head moved after it was copied.
func closeBatchLane(ctx context.Context, repo *Repo, req LandRequest, l collapseLane, b *batchRun, head string) DrainResult {
	if now := headOf(l.dir); now == "" || now != b.heads[l.name] {
		return DrainResult{Lane: l.name, Left: "lane moved after the batch was built; left queued"}
	}
	ctx = context.WithoutCancel(ctx) // the batch is applied: a cancel must not leave its lanes queued
	if path, err := laneContentIn(ctx, repo.Main, b.heads[l.name], head); path != "" {
		why := ""
		if err != nil {
			why = ": " + err.Error()
		}
		return DrainResult{Lane: l.name, Left: "content not in the batch (" + path + ")" + why + "; left queued"}
	}
	r := DrainResult{Lane: l.name, Landed: true, Batch: true}
	if err := Dequeue(l.dir); err != nil {
		fmt.Fprintf(req.Err, "worktree land: warning: land queue: %v\n", err)
		r.Warning = fmt.Sprintf("land queue: %v", err)
	}
	if req.NoClean {
		return r
	}
	res, err := removeLanded(repo, l.name, l.dir, req.Err, LedgerClose{Via: "land", LandSHA: head, Head: head})
	if why := cmp.Or(errMsg(err), res.Reason); why != "" {
		r.Warning = strings.TrimPrefix(r.Warning+"; ", "; ") + fmt.Sprintf("landed in batch %s but lane kept: %s", b.collector, why)
	}
	return r
}

// errMsg is the message of err, "" for nil.
func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// laneContentIn is "" when each path the lane changed has its tree entry at collectorHead, else the first that differs.
func laneContentIn(ctx context.Context, main, laneHead, collectorHead string) (string, error) {
	if laneHead == "" || collectorHead == "" {
		return "(no head)", errors.New("lane or batch head unknown")
	}
	mb, err := runGit(ctx, main, nil, "merge-base", "HEAD", laneHead)
	if err != nil {
		return "(merge-base)", err
	}
	files, err := laneFiles(ctx, main, strings.TrimSpace(string(mb)), laneHead)
	if err != nil {
		return "(diff)", err
	}
	for _, f := range files {
		want, err := treeEntry(ctx, main, laneHead, f)
		if err != nil {
			return f, err
		}
		got, err := treeEntry(ctx, main, collectorHead, f)
		if err != nil {
			return f, err
		}
		if want != got {
			return f, nil
		}
	}
	return "", nil
}

// deleteBranch removes the collector branch this run created once its batch landed; a failure only warns.
func (b *batchRun) deleteBranch(repo *Repo, log io.Writer) {
	if b.branch == "" {
		return
	}
	ctx, cancel := gitTimeoutCtx()
	defer cancel()
	if out, err := gitCmd(ctx, "-C", repo.Main, "branch", "-D", b.branch).CombinedOutput(); err != nil {
		fmt.Fprintf(log, "drain: warning: collector branch %s kept: %v: %s\n", b.branch, err, strings.TrimSpace(string(out)))
	}
}

// discard deletes the collector this run created, never by force when something other than its picks dirtied it.
func (b *batchRun) discard(repo *Repo, log io.Writer) {
	if b.collector == "" {
		return
	}
	left := func(why string) {
		fmt.Fprintf(log, "drain: collector %s left on disk (%s); inspect it, then worktree clean --force\n", b.collector, why)
	}
	if st, err := gitCmd(context.Background(), "-C", b.dir, "status", "--porcelain").Output(); err != nil || len(st) > 0 {
		left("it holds changes beyond the batch's picks")
		return
	}
	if cur, _ := currentWorktreeBranch(b.dir); cur != b.branch {
		left("its branch is not the one this run created")
		return
	}
	if res, err := CleanForce(repo, b.collector, false, nil); err != nil || res.Reason != "" {
		left(fmt.Sprintf("%v %s", err, res.Reason))
	}
}
