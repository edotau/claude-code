package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/serve"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/bench"
	"github.com/edotau/claude-code/internal/worktree"
)

const (
	benchCount     = "8" // benchstat needs at least 6 samples per side for a confidence interval
	benchField     = "harness_bench"
	benchWorkerArg = "--internal-bench-worker"
	benchQueueArg  = "--internal-bench-queue"
	benchPercent   = 100
	benchQuietCV   = 5
	benchMaxLines  = 4
	benchRetention = 7 * 24 * time.Hour
	benchDeadline  = 45 * time.Minute
)

// A private detached entry bypasses the hook dispatcher's five-second handler deadline.
func init() {
	if len(os.Args) == 3 && (os.Args[1] == benchWorkerArg || os.Args[1] == benchQueueArg) {
		base := os.Args[2]
		state := filepath.Join(paths.StateDir(), "bench", "post-commit")
		if filepath.Dir(base) == state && plans.ValidSHA(filepath.Base(base)) {
			if os.Args[1] == benchWorkerArg {
				benchWorker(base, true)
			} else if benchRecover(state) {
				os.Exit(1) // a reinstalled binary: the shell wrapper's loop restarts the consumer on the new code
			}
		}
		os.Exit(0)
	}
}

// benchSpawn starts the detached bench run; a seam so tests spawn nothing.
var benchSpawn = func(args []string, dir string) int { return proc.SpawnDetachedPID("sh", args, dir, nil) }

var benchPollWait = time.Second
var benchMaxBusyPolls = 30
var benchRunTimeout = benchDeadline
var benchMeasurementLock = benchLock
var benchLoopSpawn = proc.SpawnDetached
var benchLoopNow = time.Now
var benchLaneClean = bench.CleanLanes

// The worker owns lifecycle writes; fd 3 keeps its measurement lock alive if the worker dies mid-command.
const benchScript = `b=$1; r=$2; top=$3; sha=$4; load=$5; shift 5
leader=$$
(
  exec 3>&-
  sleep "${HARNESS_BENCH_DEADLINE}" & timer=$!
  trap 'kill "$timer" 2>/dev/null; exit' TERM INT
  wait "$timer"
  kill -KILL -- "-$leader"
) & watchdog=$!
trap '[ -z "${w:-}" ] || { git -C "$top" worktree remove --force "$w" >/dev/null 2>&1; rm -rf "$w"; }; kill "$watchdog" 2>/dev/null' EXIT
w=$(mktemp -d "$r.wt.XXXXXX") || { echo "bench worker: mktemp failed" >"$r.err"; exit 1; }
git -C "$top" worktree add -q --detach "$w" "$sha" >"$r.err" 2>&1 && cd "$w" ||
  { echo "bench worker: git worktree add $sha failed" >>"$r.err"; exit 1; }
HARNESS_BENCH_UNDER_LOAD=$load nice -n 19 "$b" review bench "$@" --count ` + benchCount + ` --format json >"$r" 2>"$r.err"
exit $?`

type benchJob struct {
	Top, SHA, SessionID string
	Files               []string
}

type benchMarker struct {
	Status    string    `json:"status"`
	UnderLoad bool      `json:"under_load,omitempty"`
	PID       int       `json:"pid"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Exit      *int      `json:"exit"`
	Notice    string    `json:"notice,omitempty"`
	Loop      string    `json:"loop,omitempty"`
	Retry     string    `json:"retry,omitempty"` // "wanted" after a load-inconclusive run, "done" once retried
}

var benchRun = func(args []string, top string, lock *os.File) int {
	ctx, cancel := context.WithTimeout(context.Background(), benchRunTimeout)
	defer cancel()
	shim, err := benchSnapshot(strings.TrimSuffix(args[4], ".json"))
	if err != nil {
		benchFailNote(args[4], "benchstat snapshot: "+err.Error())
		return -1
	}
	cmd := exec.CommandContext(ctx, "sh", args...)
	cmd.Dir, cmd.ExtraFiles = top, []*os.File{lock}
	cmd.Env = append(os.Environ(), "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HARNESS_BENCH_DEADLINE="+strconv.FormatFloat(benchRunTimeout.Seconds(), 'f', -1, 64))
	proc.TreeCancel(cmd, 0)
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if cmd.Process != nil {
		_ = proc.ReapGroup(cmd.Process.Pid, time.Second) // a straggler holding fd 3 keeps the measurement lock
	}
	if err == nil {
		return 0
	}
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	note := fmt.Sprintf("bench worker: exit %d: %v", code, err)
	if ctx.Err() != nil {
		note += " (deadline " + benchRunTimeout.String() + ")"
	}
	benchFailNote(args[4], note)
	return code
}

// benchFailNote appends the failure to the run's .err, so the file always names why a run failed.
func benchFailNote(result, note string) {
	f, err := os.OpenFile(result+".err", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintln(f, note) // best-effort: the marker's exit code still records the failure
}

// benchOnCommit queues a detached `review bench` for a trunk commit (a leaf's too) that changed a benched package.
func benchOnCommit(ctx context.Context, in *core.Input, dir, sha string) {
	if runtime.GOOS == "windows" || !plans.ValidSHA(sha) {
		return
	}
	top := toplevel(dir)
	if top == "" {
		return
	}
	sha = primaryTrunkHead(ctx, top, sha)
	if sha == "" {
		return
	}
	if files := benchedFiles(ctx, top, sha); len(files) > 0 {
		benchStart(ctx, in, top, sha, files)
	}
}

// BenchLanded queues every benched first-parent commit a merge or fast-forward moved the primary trunk across.
func BenchLanded(ctx context.Context, top, before, after, session string) {
	if runtime.GOOS == "windows" || !plans.ValidSHA(after) {
		return
	}
	if after = primaryTrunkHead(ctx, top, after); after == "" {
		return
	}
	shas := []string{after} // no base: only the tip is known to be new
	if plans.ValidSHA(before) {
		out, err := proc.RunGit(ctx, top, "rev-list", "--first-parent", "--reverse", before+".."+after)
		if err != nil {
			return
		}
		shas = strings.Fields(out)
	}
	if len(shas) == 0 {
		return
	}
	benched := benchedCommits(ctx, top, shas)
	var jobs []benchJob
	for _, sha := range shas {
		if files := benched[sha]; len(files) > 0 {
			jobs = append(jobs, benchJob{top, sha, session, files})
		}
	}
	benchEnqueue(top, jobs)
}

// Admission and lock waits belong to the detached worker, never the tool's hook process.
func benchStart(_ context.Context, in *core.Input, top, sha string, files []string) {
	benchEnqueue(top, []benchJob{{top, sha, in.SessionID, files}})
}

// benchEnqueue publishes jobs in landing order; one consumer runs them oldest first, so none is dropped.
func benchEnqueue(top string, jobs []benchJob) {
	state := filepath.Join(paths.StateDir(), "bench", "post-commit")
	if len(jobs) == 0 || os.MkdirAll(state, 0o700) != nil {
		return
	}
	bin, err := paths.SelfBinary()
	if err != nil {
		return
	}
	// Start recovery before publishing: a hook killed after the atomic write cannot strand its job.
	args := []string{"-c", `while :; do "$1" "$2" "$3" && exit; sleep 1; done`, "sh", bin, benchQueueArg,
		filepath.Join(state, jobs[0].SHA)}
	if benchSpawn(args, top) == 0 {
		return
	}
	for _, job := range jobs {
		base := filepath.Join(state, job.SHA)
		raw, err := os.ReadFile(base + ".job.json")
		var prior benchJob
		if errors.Is(err, os.ErrNotExist) || err == nil && json.Unmarshal(raw, &prior) != nil {
			_ = paths.WriteJSON(base+".job.json", job)
		}
	}
}

// One persistent detached consumer per config reclaims jobs even when no later commit arrives; true asks a restart.
func benchRecover(state string) bool {
	lock, err := os.OpenFile(filepath.Join(state, "queue.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return true
	}
	defer lock.Close()
	if !paths.TryLock(lock) {
		return false
	}
	self := benchBinaryStamp()
	for self == "" || benchBinaryStamp() == self {
		benchReconcile(state)
		benchConsume(state)
		time.Sleep(benchPollWait)
	}
	return true
}

// benchConsume makes one pass over the queue; the first run's load gate is read once, and only when a retry is due.
func benchConsume(state string) {
	quiet := sync.OnceValue(func() bool { return !benchBusy() })
	for _, base := range benchQueued(state) {
		if benchPending(base) {
			benchWorker(base, false)
		} else if job, ok := benchUndelivered(base); ok {
			benchDeliver(base, job)
		} else if benchRetryDue(base) && quiet() {
			benchWorker(base, true)
		}
	}
}

// benchBinaryStamp identifies the installed binary, so a consumer never outlives a reinstall on old code.
func benchBinaryStamp() string {
	bin, err := paths.SelfBinary()
	if err != nil {
		return ""
	}
	fi, err := os.Stat(bin)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
}

// benchQueued lists job bases oldest published first, so a landed range runs in commit order.
func benchQueued(state string) []string {
	files, _ := filepath.Glob(filepath.Join(state, "*.job.json"))
	type queued struct {
		base string
		at   time.Time
	}
	jobs := make([]queued, 0, len(files))
	for _, file := range files {
		if fi, err := os.Stat(file); err == nil {
			jobs = append(jobs, queued{strings.TrimSuffix(file, ".job.json"), fi.ModTime()})
		}
	}
	slices.SortStableFunc(jobs, func(a, b queued) int { return a.at.Compare(b.at) })
	bases := make([]string, len(jobs))
	for i, j := range jobs {
		bases[i] = j.base
	}
	return bases
}

// benchRetryDue is a finished, load-inconclusive run whose one retry has not run; the consumer delivers first.
func benchRetryDue(base string) bool {
	m, ok := benchReadMarker(base)
	return ok && m.Exit != nil && m.Retry == "wanted"
}

func benchReadMarker(base string) (benchMarker, bool) {
	var m benchMarker
	raw, err := os.ReadFile(base + ".marker")
	return m, err == nil && json.Unmarshal(raw, &m) == nil
}

func benchPending(base string) bool {
	raw, err := os.ReadFile(base + ".marker")
	var m benchMarker
	return err != nil || json.Unmarshal(raw, &m) != nil || m.Exit == nil
}

// A worker that died between completion and delivery leaves an exit with no notice outcome.
func benchUndelivered(base string) (benchJob, bool) {
	var m benchMarker
	var job benchJob
	raw, err := os.ReadFile(base + ".marker")
	if err != nil || json.Unmarshal(raw, &m) != nil || m.Exit == nil || m.Notice != "" {
		return job, false
	}
	if m.PID > 0 && m.PID != os.Getpid() && proc.Alive(m.PID) {
		return job, false
	}
	raw, err = os.ReadFile(base + ".job.json")
	if err != nil || json.Unmarshal(raw, &job) != nil || job.SessionID == "" {
		return job, false
	}
	return job, true
}

func benchDeliver(base string, job benchJob) {
	in := core.ParseInput(strings.NewReader(fmt.Sprintf(`{"session_id":%q,"harness_bench":{"result":%q,"sha":%q}}`,
		job.SessionID, base+".json", job.SHA)))
	benchFinish(in)
}

func benchComplete(base string, m benchMarker, code int) error {
	m.Status, m.End, m.Exit = "failed", time.Now().UTC(), &code
	if code == 0 {
		m.Status = "done"
	}
	return paths.WriteJSON(base+".marker", m)
}

func benchReconcile(state string) {
	markers, _ := filepath.Glob(filepath.Join(state, "*.marker"))
	for _, file := range markers {
		raw, err := os.ReadFile(file)
		var m benchMarker
		if err != nil {
			continue
		}
		if json.Unmarshal(raw, &m) != nil {
			if strings.TrimSpace(string(raw)) != "running" {
				continue
			}
			m.Status = "running"
		}
		if m.abandoned() {
			m.Status, m.PID, m.Exit = "queued", 0, nil
			_ = paths.WriteJSON(file, m)
		}
		if (m.Status == "done" || m.Status == "failed") && !m.End.IsZero() && time.Since(m.End) > benchRetention {
			benchPrune(strings.TrimSuffix(file, ".marker"))
		}
	}
}

func benchPrune(base string) {
	trees, _ := filepath.Glob(base + ".json.wt.*") // a SIGKILLed worker's EXIT trap never removed these
	for _, file := range append(trees, base+".job.json", base+".in.json", base+".json", base+".json.err",
		base+".baseline", base+".marker") {
		if err := os.RemoveAll(file); err != nil {
			return
		}
	}
}

func (m benchMarker) abandoned() bool {
	if m.Status != "running" && m.Status != "queued" {
		return false
	}
	if m.PID > 0 {
		return !proc.Alive(m.PID)
	}
	return time.Since(m.Start) > time.Minute
}

// benchRetryState is "done" when a prior run asked for or used its retry: this run is that retry, or a manual rerun.
func benchRetryState(base string) string {
	if prior, _ := benchReadMarker(base); prior.Retry != "" {
		return "done"
	}
	return ""
}

// benchNextRetry asks for the one retry when a first run measured fine but the load gate voided it.
func benchNextRetry(retry string, code int, base string) string {
	if retry == "" && code == 0 && benchResultLoaded(base+".json") {
		return "wanted"
	}
	return retry
}

func benchWorker(base string, rerun bool) {
	raw, err := os.ReadFile(base + ".job.json")
	var job benchJob
	if err != nil || json.Unmarshal(raw, &job) != nil || !plans.ValidSHA(job.SHA) {
		return
	}
	m := benchMarker{Status: "queued", PID: os.Getpid(), Start: time.Now().UTC(), Retry: benchRetryState(base)}
	lock, err := benchMeasurementLock()
	if err != nil {
		_ = benchComplete(base, m, -1)
		return
	}
	defer lock.Close()
	for !paths.TryLock(lock) {
		time.Sleep(benchPollWait)
	}
	if !rerun && !benchPending(base) {
		return
	}
	busy := benchWaitQuiet() // a retry passes the same gate; the consumer started it only once that gate was quiet
	m.UnderLoad = busy
	m.Status, m.Start = "running", time.Now().UTC()
	if paths.WriteJSON(base+".marker", m) != nil {
		return
	}
	code := benchMeasure(base, job, lock, busy)
	m.Retry = benchNextRetry(m.Retry, code, base)
	if benchComplete(base, m, code) != nil {
		return
	}
	benchDeliver(base, job)
	err = lock.Close()
	_ = benchDecideLoop(base, job, err)
}

// The marker is the worker's only log: one loop decision per measured job, never a path or command.
func benchDecideLoop(base string, job benchJob, lockErr error) error {
	raw, err := os.ReadFile(base + ".marker")
	if err != nil {
		return err
	}
	var m benchMarker
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m.Loop = "measurement lock close failed"
	if lockErr == nil {
		m.Loop = benchLoopDecision(context.Background(), job)
	}
	return paths.WriteJSON(base+".marker", m)
}

func benchLoopDecision(ctx context.Context, job benchJob) string {
	now := benchLoopNow()
	if primaryTrunkHead(ctx, job.Top, job.SHA) == "" {
		return "not primary trunk"
	}
	item, ok, err := bench.NextCandidateFor(job.Top, now, nil)
	if err != nil {
		return "candidate lookup failed"
	} else if !ok {
		return "no candidate for this repo"
	}
	pkg := filepath.FromSlash(item.Pkg)
	if fi, err := os.Stat(filepath.Join(job.Top, pkg)); pkg == "." || !filepath.IsLocal(pkg) || err != nil || !fi.IsDir() {
		return "candidate not in this repo" // the history is machine-wide; another repo's commit must not fix a package it lacks
	}
	if benchBusy() {
		return "machine busy"
	}
	benchCleanLanes(ctx, job)
	return benchLaunchLoop(job, now)
}

// Merged lanes free their cap slots first; a clean failure is a notice and never stops the decision.
func benchCleanLanes(ctx context.Context, job benchJob) {
	var out strings.Builder
	if err := benchLaneClean(ctx, job.Top, &out); err != nil {
		fmt.Fprintf(&out, "automatic lane clean failed: %s\n", strings.SplitN(err.Error(), "\n", 2)[0])
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if line == "" || job.SessionID == "" {
			continue
		}
		// best-effort: a lost lane notice never blocks the loop decision, and `review bench --status` lists the lanes
		_ = worktree.Append(job.SessionID, worktree.Notice{TS: time.Now().Unix(), From: "bench", Kind: "bench", Text: line})
	}
}

// One launch row per commit, reserved atomically before the spawn, so a crash between them still counts.
func benchLaunchLoop(job benchJob, now time.Time) string {
	bin, err := paths.SelfBinary()
	if err != nil {
		return "binary lookup failed"
	}
	if ok, why := bench.Reserve(bench.KindBenchLoop, job.SHA, now); !ok {
		return "budget refused: " + why
	}
	if !benchLoopSpawn(bin, []string{"review", "bench", "--loop", "--max", "1", "--reserved", job.SHA}, job.Top, nil) {
		return "loop launch failed"
	}
	return "loop launched"
}

// The loop driver takes the same lock, so a post-commit measurement never overlaps its A/B runs.
func benchLock() (*os.File, error) {
	path, err := bench.MeasurementLockPath()
	if err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

func benchWaitQuiet() bool {
	busy := benchBusy()
	for polls := 0; busy && polls < benchMaxBusyPolls; polls++ {
		time.Sleep(benchPollWait)
		busy = benchBusy()
	}
	return busy
}

var benchBusy = func() bool {
	running := 0
	status, err := serve.QueryStatus(time.Second)
	if err != nil && !errors.Is(err, serve.ErrNotRunning) {
		return true
	}
	if err == nil {
		for _, pool := range status.Pools {
			running += pool.Running
		}
	}
	quiet, _, err := proc.QuietMachine(context.Background(), running)
	if err != nil || !quiet {
		return true
	}
	return inflight.Count().Total > 0
}

func benchMeasure(base string, job benchJob, lock *os.File, underLoad bool) int {
	bin, err := paths.SelfBinary()
	if err != nil {
		benchFailNote(base+".json", "bench worker: binary lookup: "+err.Error())
		return -1
	}
	top, sha, files := job.Top, job.SHA, job.Files
	// The runner sees admission load before writing history; --repo names the repo, not the temporary worktree.
	args := []string{"-c", benchScript, "sh", bin, base + ".json", top, sha, strconv.FormatBool(underLoad), "--repo", top}
	for _, p := range files {
		rel, err := filepath.Rel(top, p)
		if err != nil {
			rel = p
		}
		args = append(args, "--path", rel)
	}
	code := benchRun(args, top, lock)
	benchCleanTrees(top, base+".json")
	if code != 0 {
		return code
	}
	if code = benchAnnotate(base, benchBaselines(job, base+".baseline"), underLoad); code != 0 {
		benchFailNote(base+".json", "bench worker: result missing or not a bench report")
		return code
	}
	_ = os.Remove(base + ".json.err") // success: its stderr held only progress lines
	return 0
}

// benchResultLoaded reports a run whose comparisons the load gate voids: the case a quiet retry can fix.
func benchResultLoaded(result string) bool {
	raw, err := os.ReadFile(result)
	var rep benchReport
	if err != nil || json.Unmarshal(raw, &rep) != nil {
		return false
	}
	return slices.ContainsFunc(rep.Packages, rep.loaded)
}

func (r benchReport) loaded(p benchPackage) bool {
	return r.NoisyRun || r.CPUs > 0 && max(p.Stats.LoadBefore, p.Stats.LoadAfter) > float64(r.CPUs)/2
}

// A deadline SIGKILL skips the script's EXIT trap, so the worker removes the detached worktrees it left.
func benchCleanTrees(top, result string) {
	dir, prefix := filepath.Dir(result), filepath.Base(result)+".wt."
	entries, _ := os.ReadDir(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	found := false
	for _, e := range entries {
		if tree := filepath.Join(dir, e.Name()); strings.HasPrefix(e.Name(), prefix) {
			_, _ = proc.RunGit(ctx, top, "worktree", "remove", "--force", tree)
			_ = os.RemoveAll(tree)
			found = true
		}
	}
	if found {
		_, _ = proc.RunGit(ctx, top, "worktree", "prune")
	}
}

// Capture the exact benchstat input without redirecting the runner's history or rolling-baseline writes.
func benchSnapshot(base string) (string, error) {
	bin, err := exec.LookPath("benchstat")
	if err != nil {
		return "", err
	}
	dir := base + ".baseline"
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nset -eu\nbin=" + quote(bin) + "\ndir=" + quote(dir) + `
test "$#" -eq 4 && test "$1" = -format && test "$2" = csv
pkg=$(basename "$(dirname "$3")")
snapshot="$dir/$pkg.txt"
cp "$3" "$snapshot"
exec "$bin" "$1" "$2" "$snapshot" "$4"
`
	return dir, paths.AtomicWriteNoSync(filepath.Join(dir, "benchstat"), []byte(script), 0o700)
}

// HEAD may have advanced past sha when the asynchronous commit hook runs.
func primaryTrunkHead(ctx context.Context, top, sha string) string {
	out, err := proc.RunGit(ctx, top, "rev-parse", sha+"^{commit}", "--git-dir", "--git-common-dir")
	rows := strings.Fields(out)
	if err != nil || len(rows) != 3 || !strings.HasPrefix(rows[0], sha) || rows[1] != rows[2] {
		return ""
	}
	branch, err := proc.RunGit(ctx, top, "symbolic-ref", "--short", "HEAD")
	if b := strings.TrimSpace(branch); err != nil || b != "main" && b != "master" {
		return ""
	}
	if _, err := proc.RunGit(ctx, top, "merge-base", "--is-ancestor", rows[0], "HEAD"); err != nil {
		return ""
	}
	return rows[0]
}

// benchedFiles are the commit's changed Go files whose package dir held a Benchmark func in that commit.
func benchedFiles(ctx context.Context, top, sha string) []string {
	for full, files := range benchedCommits(ctx, top, []string{sha}) {
		if strings.HasPrefix(full, sha) {
			return files // git show names it by its full sha, whatever form the caller passed
		}
	}
	return nil
}

// benchedCommits maps each commit to its benched changed files, judged in the commit's own tree, in two git calls.
func benchedCommits(ctx context.Context, top string, shas []string) map[string][]string {
	changed := benchChangedGo(ctx, top, shas)
	var specs, revs []string // revs are full shas, so grep's "rev:path" prefixes match changed's keys
	for sha, files := range changed {
		revs = append(revs, sha)
		for _, f := range files {
			if spec := path.Join(path.Dir(f), "*_test.go"); !slices.Contains(specs, spec) {
				specs = append(specs, spec)
			}
		}
	}
	out := map[string][]string{}
	if len(specs) == 0 {
		return out
	}
	args := append(append([]string{"grep", "-l", "^func Benchmark"}, revs...), "--")
	// exit 1 is "no match": no commit holds a benchmark in a changed dir
	grep, _ := proc.RunGit(ctx, top, append(args, specs...)...)
	benched := map[string]bool{} // "sha:dir" pairs whose test files hold a Benchmark
	for line := range strings.SplitSeq(grep, "\n") {
		if sha, file, ok := strings.Cut(strings.TrimSpace(line), ":"); ok {
			benched[sha+":"+path.Dir(file)] = true
		}
	}
	for sha, files := range changed {
		for _, f := range files {
			if benched[sha+":"+path.Dir(f)] {
				out[sha] = append(out[sha], filepath.Join(top, filepath.FromSlash(f)))
			}
		}
	}
	return out
}

// benchChangedGo is each commit's changed .go paths; a merge lists what it brought in, not only its conflicts.
func benchChangedGo(ctx context.Context, top string, shas []string) map[string][]string {
	args := append([]string{"show", "--name-only", "--no-renames", "--diff-merges=first-parent", "--format=%x01%H"}, shas...)
	raw, err := proc.RunGit(ctx, top, args...)
	changed := map[string][]string{}
	if err != nil {
		return changed
	}
	sha := ""
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if h, ok := strings.CutPrefix(line, "\x01"); ok {
			sha = h
		} else if sha != "" && strings.HasSuffix(line, ".go") {
			changed[sha] = append(changed[sha], line)
		}
	}
	return changed
}

type benchDelta struct {
	Change      string `json:"change"`
	P           string `json:"p"`
	Significant bool   `json:"significant"`
}

type benchReport struct {
	Commit   string         `json:"commit"`
	CPUs     int            `json:"cpus"`
	NoisyRun bool           `json:"noisy_load"`
	Packages []benchPackage `json:"packages"`
}

type benchPackage struct {
	Pkg      string        `json:"pkg"`
	Compared bool          `json:"compared"`
	Missing  []string      `json:"missing"`
	Before   benchBaseline `json:"hook_baseline"`
	Stats    struct {
		LoadBefore float64 `json:"load_before"`
		LoadAfter  float64 `json:"load_after"`
	} `json:"stats"`
	Benchmarks []struct {
		Name   string                `json:"name"`
		Flags  []string              `json:"flags"`
		Deltas map[string]benchDelta `json:"deltas"`
	} `json:"benchmarks"`
}

type benchBaseline struct {
	Commit string            `json:"commit"`
	Reason string            `json:"reason,omitempty"`
	Flags  map[string]string `json:"flags"`
}

func benchBaselines(job benchJob, root string) map[string]benchBaseline {
	out := map[string]benchBaseline{}
	for _, file := range job.Files {
		rel, err := filepath.Rel(job.Top, filepath.Dir(file))
		if err != nil {
			continue
		}
		pkg := filepath.ToSlash(rel)
		if _, seen := out[pkg]; seen {
			continue
		}
		name := strings.ReplaceAll(strings.TrimPrefix(pkg, "internal/"), "/", "-")
		if pkg == "." {
			name = "root"
		}
		raw, err := os.ReadFile(filepath.Join(root, name+".txt"))
		if err != nil {
			out[pkg] = benchBaseline{Commit: "none", Reason: "baseline unavailable"}
			continue
		}
		out[pkg] = benchBaselineText(string(raw))
	}
	return out
}

func benchBaselineText(text string) benchBaseline {
	b := benchBaseline{Commit: "unknown", Flags: map[string]string{}}
	values := map[string][]float64{}
	headers := map[string]string{}
	for line := range strings.SplitSeq(text, "\n") {
		f := strings.Fields(strings.TrimPrefix(line, "# "))
		if len(f) < 2 {
			continue
		}
		headers[f[0]] = f[1]
		benchSample(values, f)
	}
	if headers["commit:"] != "" {
		b.Commit = headers["commit:"]
	}
	b.Reason = benchBaselineLoad(headers)
	for name, samples := range values {
		b.Flags[name] = benchSampleReason(samples)
	}
	return b
}

func benchBaselineLoad(headers map[string]string) string {
	values := map[string]float64{}
	for _, key := range []string{"cpus:", "load-before:", "load-after:"} {
		v, err := strconv.ParseFloat(headers[key], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return "baseline metadata unavailable"
		}
		values[key] = v
	}
	if values["cpus:"] == 0 || headers["commit:"] == "" {
		return "baseline metadata unavailable"
	}
	if max(values["load-before:"], values["load-after:"]) > values["cpus:"]/2 {
		return "baseline load"
	}
	return ""
}

func benchSample(values map[string][]float64, fields []string) {
	if !strings.HasPrefix(fields[0], "Benchmark") {
		return
	}
	for i := 3; i < len(fields); i += 2 {
		if v, err := strconv.ParseFloat(fields[i-1], 64); fields[i] == "ns/op" && err == nil {
			values[fields[0]] = append(values[fields[0]], v)
		}
	}
}

func benchSampleReason(samples []float64) string {
	if len(samples) < 2 {
		return "baseline insufficient samples"
	}
	mean := 0.0
	for _, v := range samples {
		mean += v / float64(len(samples))
	}
	variance := 0.0
	for _, v := range samples {
		variance += (v - mean) * (v - mean) / float64(len(samples)-1)
	}
	if mean <= 0 || math.IsNaN(mean) || math.IsInf(mean, 0) {
		return "baseline invalid samples"
	}
	cv := math.Sqrt(variance) / mean * benchPercent
	if math.Round(cv*benchPercent)/benchPercent > benchQuietCV {
		return "baseline noisy"
	}
	return ""
}

func benchAnnotate(base string, before map[string]benchBaseline, underLoad bool) int {
	raw, err := os.ReadFile(base + ".json")
	if err != nil {
		return -1
	}
	// Preserve the runner's full evidence, including fields this hook does not interpret.
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil {
		return -1
	}
	if underLoad {
		document["noisy_load"] = json.RawMessage("true")
	}
	var packages []map[string]json.RawMessage
	if json.Unmarshal(document["packages"], &packages) != nil {
		return -1
	}
	for i := range packages {
		var pkg string
		if json.Unmarshal(packages[i]["pkg"], &pkg) != nil {
			return -1
		}
		baseline := before[pkg]
		if baseline.Commit == "" {
			baseline = benchBaseline{Commit: "unknown", Reason: "baseline metadata unavailable"}
		}
		packages[i]["hook_baseline"], _ = json.Marshal(baseline)
	}
	document["packages"], _ = json.Marshal(packages)
	if paths.WriteJSON(base+".json", document) != nil {
		return -1
	}
	return 0
}

// TickOrBench is the docs-plan-tick module: a finished bench run's payload posts its notices, else Tick.
func TickOrBench(in *core.Input) core.Result {
	if in.ToolName == "Bash" && in.Nested(benchField, "result") != "" {
		return benchFinish(in)
	}
	return Tick(in)
}

// benchFinish turns the finished run's result file into notices on the committing session's next prompt.
func benchFinish(in *core.Input) core.Result {
	result := in.Nested(benchField, "result")
	base := strings.TrimSuffix(result, ".json")
	markerRaw, err := os.ReadFile(base + ".marker")
	var m benchMarker
	if err != nil || json.Unmarshal(markerRaw, &m) != nil || m.Exit == nil || in.SessionID == "" {
		return core.Result{}
	}
	raw, _ := os.ReadFile(result)
	lines := benchNoticeLines(raw)
	if len(lines) > benchMaxLines {
		lines = append(lines[:benchMaxLines-1], benchNoticeSummary(raw, len(lines)-benchMaxLines+1))
	}
	if *m.Exit != 0 {
		lines = []string{fmt.Sprintf("bench %s vs unknown: inconclusive (run failed, exit %d)", in.Nested(benchField, "sha"), *m.Exit)}
	}
	if len(lines) == 0 {
		lines = []string{fmt.Sprintf("bench %s vs unknown: inconclusive (result unavailable or empty); rerun wanted", in.Nested(benchField, "sha"))}
	}
	m.Notice = "delivered"
	for _, line := range lines {
		quoted := "'" + strings.ReplaceAll(base, "'", "'\\''") + "'"
		line += fmt.Sprintf("; result %s; rerun: claude-code %s %s", result, benchWorkerArg, quoted)
		if err := worktree.Append(in.SessionID, worktree.Notice{TS: time.Now().Unix(), From: "bench", Kind: "bench", Text: line}); err != nil {
			m.Notice = "failed: " + err.Error()
		}
	}
	_ = paths.WriteJSON(base+".marker", m)
	return core.Result{}
}

var benchSuffix = regexp.MustCompile(`-\d+$`)

// Every comparison retains its own provenance and reason, including missing comparisons.
func benchNoticeLines(raw []byte) []string {
	var rep benchReport
	if json.Unmarshal(raw, &rep) != nil {
		return nil
	}
	var lines []string
	for _, p := range rep.Packages {
		baseline := p.Before.Commit
		if baseline == "" {
			baseline = "unknown"
		}
		prefix := fmt.Sprintf("bench %s vs %s %s: ", rep.Commit, baseline, p.Pkg)
		for _, b := range p.Benchmarks {
			reason := p.baselineReason(b.Name)
			flags := slices.Clone(b.Flags)
			if reason != "" {
				flags = append(flags, reason)
			}
			parts := benchParts(rep.loaded(p), flags, b.Deltas)
			name := benchSuffix.ReplaceAllString(b.Name, "")
			lines = append(lines, prefix+name+" "+strings.Join(parts, "; "))
		}
		for _, missing := range p.Missing {
			lines = append(lines, prefix+"inconclusive ("+missing+"); rerun wanted")
		}
	}
	slices.SortStableFunc(lines, func(a, b string) int { return benchNoticeRank(a) - benchNoticeRank(b) })
	return lines
}

func benchNoticeRank(line string) int {
	switch {
	case strings.Contains(line, "inconclusive"):
		return 0
	case strings.Contains(line, "ns +"), strings.Contains(line, "allocs +"), strings.Contains(line, "B +"):
		return 1
	case strings.Contains(line, "no significant change"):
		return 3
	default:
		return 2
	}
}

func benchNoticeSummary(raw []byte, remaining int) string {
	var rep benchReport
	_ = json.Unmarshal(raw, &rep)
	var baselines []string
	for _, p := range rep.Packages {
		baseline := p.Before.Commit
		if baseline == "" {
			baseline = "unknown"
		}
		if !slices.Contains(baselines, baseline) {
			baselines = append(baselines, baseline)
		}
	}
	return fmt.Sprintf("bench %s vs %s: %d more comparisons; full evidence in result", rep.Commit, strings.Join(baselines, ","), remaining)
}

func (p benchPackage) baselineReason(name string) string {
	if !p.Compared {
		return "no comparable baseline"
	}
	if p.Before.Reason != "" {
		return p.Before.Reason
	}
	if reason, ok := p.Before.Flags[name]; ok {
		return reason
	}
	return "baseline sample unavailable"
}

func benchQuietReason(noisyRun bool, flags []string) string {
	if noisyRun || slices.Contains(flags, "load") {
		return "load"
	}
	if slices.Contains(flags, "noisy") {
		return "noisy"
	}
	for _, flag := range flags {
		if strings.HasPrefix(flag, "baseline ") || flag == "no comparable baseline" {
			return flag
		}
	}
	return ""
}

// The same evidence gate covers every metric and both directions.
func benchParts(noisyRun bool, flags []string, deltas map[string]benchDelta) []string {
	if reason := benchQuietReason(noisyRun, flags); reason != "" {
		return []string{"inconclusive (" + reason + "); rerun wanted"}
	}
	if len(deltas) == 0 {
		return []string{"inconclusive (missing comparison); rerun wanted"}
	}
	var parts []string
	for _, u := range []struct{ unit, label string }{{"allocs/op", "allocs"}, {"B/op", "B"}, {"sec/op", "ns"}} {
		d, ok := deltas[u.unit]
		if !ok || !d.Significant {
			continue
		}
		pv, _, _ := strings.Cut(d.P, " ")
		parts = append(parts, fmt.Sprintf("%s %s (%s)", u.label, d.Change, pv))
	}
	if len(parts) == 0 {
		parts = append(parts, "not judged (no significant change)")
	}
	return parts
}

type benchTally struct {
	benched, failed, queued, skipped, without, retries int
	regression                                         string
}

// BenchCoverage is the doctor row over top's last n trunk commits; ok is false on a failed run or a regression.
func BenchCoverage(ctx context.Context, top string, n int) (detail string, ok bool) {
	out, err := proc.RunGit(ctx, top, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", n), "HEAD")
	if err != nil {
		return "no git history at " + top, true
	}
	state := filepath.Join(paths.StateDir(), "bench", "post-commit")
	var t benchTally
	shas := strings.Fields(out)
	benched := benchedCommits(ctx, top, shas)
	for _, sha := range shas {
		if len(benched[sha]) == 0 {
			t.without++
			continue
		}
		t.add(filepath.Join(state, sha))
	}
	detail = fmt.Sprintf("last %d commits: %d benched, %d failed, %d queued, %d skipped, %d without benchmarks",
		len(shas), t.benched, t.failed, t.queued, t.skipped, t.without)
	if t.retries > 0 {
		detail += fmt.Sprintf("; %d await a quiet retry", t.retries)
	}
	if t.regression == "" {
		return detail + "; no regression", t.failed == 0
	}
	return detail + "; latest regression: " + t.regression, false
}

// add counts one benched commit; commits arrive newest first, so the first regression found is the latest.
func (t *benchTally) add(base string) {
	m, ok := benchReadMarker(base)
	switch {
	case ok && m.Exit != nil && *m.Exit != 0:
		t.failed++
	case ok && m.Exit != nil:
		t.benched++
		if m.Retry == "wanted" {
			t.retries++
		}
		if t.regression == "" {
			t.regression = benchRegression(base + ".json")
		}
	case paths.FileExists(base + ".job.json"):
		t.queued++
	default:
		t.skipped++
	}
}

// benchRegression is the result's first judged increase, the same line its notice carried.
func benchRegression(result string) string {
	raw, err := os.ReadFile(result)
	if err != nil {
		return ""
	}
	for _, line := range benchNoticeLines(raw) {
		if benchNoticeRank(line) == 1 {
			return line
		}
	}
	return ""
}
