package bench

import (
	"bytes"
	"cmp"
	"context"
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

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/source"
)

const (
	minScoreMSDay = 1000 // design §5: a candidate under 1 s/day is not worth a lane
	maxReverts    = 2
	abRounds      = 4
	abCount       = 2 // 4 rounds x 2 = 8 samples per side, benchstat's 6 plus margin
	leafWait      = "45m"
	ownerFlag     = "OWNER REVIEW REQUIRED"
	maxSubject    = 100
)

type outcome int

const (
	kept outcome = iota
	reverted
	held
	noComparison
	deferred
)

// driver runs `agents ask` and `worktree` as subprocesses of this binary: internal/agent and worktree import bench.
type driver struct {
	top, exe string
	out, log io.Writer
	skip     map[string]bool // items held for the owner this run; unrecorded, so the queue still ranks them
	steps    loopSteps
}

// loopSteps are the loop's three stages, fields so a test can replace them.
type loopSteps struct {
	quiet func(context.Context) (bool, string)
	pick  func() (Item, bool, error)
	item  func(context.Context, Item) (outcome, error)
}

var (
	newDriverFn       = newDriver
	driverRun         = execRun // the subprocess runner; a test swaps it
	errJobActive      = errors.New("background job cancellation failed")
	errParentWorktree = errors.New("parent worktree removal failed")
	errMeasureBusy    = errors.New("benchmark measurement lock busy")
	measureLockPath   = MeasurementLockPath // a test swaps it
	measureWait       = 50 * time.Minute    // past the post-commit worker's 45 minute deadline
)

// MeasurementLockPath is the uid-scoped machine-wide lock every benchmark measurement holds, created 0700.
func MeasurementLockPath() (string, error) {
	dir := filepath.Join("/tmp", "claude-code-bench-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	uid, ok := paths.OwnerUID(fi)
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 || !ok || uid != os.Getuid() {
		return "", fmt.Errorf("unsafe benchmark lock directory %s", dir)
	}
	return filepath.Join(dir, "measurement.lock"), nil
}

// measured runs fn holding the measurement lock the post-commit worker shares, so the two never overlap.
func measured(fn func() error) error {
	path, err := measureLockPath()
	if err != nil {
		return err
	}
	ran, err := paths.TryFileLock(path, measureWait, fn)
	if err == nil && !ran {
		err = errMeasureBusy
	}
	return err
}

// candidate is one queue item's lane and the evidence gathered for it.
type candidate struct {
	Item
	slug, lane, work, before string
	parent, fixSHA, fixID    string
	files                    []string
	res                      ABResult
}

type askEnvelope struct {
	Status     string `json:"status"`
	Model      string `json:"model"`
	DispatchID string `json:"dispatch_id"`
	Answer     string `json:"answer"`
}

func runDriver(l loopOpts, stdout, stderr io.Writer) int {
	d, err := newDriverFn(stdout, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "review bench --loop:", err)
		return 1
	}
	if err := os.MkdirAll(benchRoot(), 0o755); err != nil {
		fmt.Fprintln(stderr, "review bench --loop:", err)
		return 1
	}
	ran, err := paths.TryFileLock(filepath.Join(benchRoot(), "loop.lock"), 0, func() error {
		return d.loop(context.Background(), l.maxItems)
	})
	switch {
	case err != nil:
		fmt.Fprintln(stderr, "review bench --loop:", err)
		return 1
	case !ran:
		fmt.Fprintln(stdout, "bench loop: another loop is running; deferred")
	}
	return 0
}

func newDriver(stdout, stderr io.Writer) (*driver, error) {
	if _, err := exec.LookPath("benchstat"); err != nil {
		return nil, errors.New("benchstat not on PATH (go install golang.org/x/perf/cmd/benchstat@latest)")
	}
	top, err := repoTop()
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	d := &driver{top: top, exe: exe, out: stdout, log: stderr, skip: map[string]bool{}}
	d.steps = loopSteps{quiet: d.quiet, pick: d.pick, item: d.item}
	return d, nil
}

func (d *driver) loop(ctx context.Context, maxItems int) error {
	reverts := 0
	for range maxItems {
		if reverts >= maxReverts {
			fmt.Fprintf(d.out, "bench loop: %d reverts in a row; stopping\n", reverts)
			return nil
		}
		if quiet, why := d.steps.quiet(ctx); !quiet {
			fmt.Fprintf(d.out, "bench loop: machine busy (%s); deferred\n", why)
			return nil
		}
		it, ok, err := d.steps.pick()
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(d.out, "bench loop: no fix candidate at 1 s/day or more; stopping")
			return nil
		}
		o, err := d.steps.item(ctx, it)
		if err != nil {
			return err
		}
		switch o {
		case noComparison:
			fmt.Fprintln(d.out, "bench loop: no conclusive comparison; item dropped, stopping")
			return nil
		case deferred:
			fmt.Fprintln(d.out, "bench loop: measurement lock busy; deferred")
			return nil
		case reverted:
			reverts++
		default:
			reverts = 0
		}
	}
	fmt.Fprintf(d.out, "bench loop: done (--max %d)\n", maxItems)
	return nil
}

// quiet is step 1: load at most half the CPUs and no model job running, served or in flight.
func (d *driver) quiet(ctx context.Context) (bool, string) {
	running := 0
	out, code, err := d.run(ctx, d.top, "", d.exe, "agents", "serve", "--status", "--format", "json")
	switch {
	case code == 1: // no daemon: background jobs run directly and show in the in-flight count
	case err != nil:
		return false, "serve status: " + err.Error()
	default:
		var st struct {
			Pools []struct {
				Running int `json:"running"`
			} `json:"pools"`
		}
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			return false, "serve status: " + err.Error()
		}
		for _, p := range st.Pools {
			running += p.Running
		}
	}
	running += inflight.Count().Total
	ok, load, err := proc.QuietMachine(ctx, running)
	if err != nil {
		return false, err.Error()
	}
	return ok, fmt.Sprintf("load %.2f, %d dispatches running", load, running)
}

// pick is step 2: the best fix item with a benchmark, skipping items held this run; ranked by score.
func (d *driver) pick() (Item, bool, error) {
	var buf bytes.Buffer
	if err := runQueue(loopOpts{kind: "fix"}, true, &buf); err != nil {
		return Item{}, false, err
	}
	var items []Item
	if err := json.Unmarshal(buf.Bytes(), &items); err != nil {
		return Item{}, false, err
	}
	for _, it := range items {
		if it.score() < minScoreMSDay {
			break
		}
		if !d.skip[closeKey(it.Pkg, it.What)] && hasBenchmarks(filepath.Join(d.top, it.Pkg)) {
			return it, true, nil
		}
	}
	return Item{}, false, nil
}

// item runs steps 3-12 for one candidate.
func (d *driver) item(ctx context.Context, it Item) (outcome, error) {
	c, err := d.openLane(ctx, it)
	if errors.Is(err, errMeasureBusy) {
		return deferred, nil // openLane already cleaned the lane
	}
	if err != nil {
		return 0, err
	}
	fmt.Fprintf(d.out, "bench loop: %s: %s in lane %s\n", it.Pkg, it.What, c.slug)
	if err := d.fixLeaf(ctx, &c); err != nil {
		if errors.Is(err, errJobActive) {
			return d.hold(c, err.Error())
		}
		return 0, errors.Join(err, d.clean(ctx, c))
	}
	why, err := d.gates(ctx, &c)
	if err != nil {
		return 0, errors.Join(err, d.clean(ctx, c))
	}
	if why != "" {
		return d.revert(ctx, c, why)
	}
	if err := d.interleave(ctx, &c); err != nil {
		if errors.Is(err, errParentWorktree) || errors.Is(err, errMeasureBusy) {
			return d.hold(c, err.Error())
		}
		return 0, errors.Join(err, d.clean(ctx, c))
	}
	return d.verdict(ctx, c)
}

// verdict acts on the decision: clean a no-comparison, revert a loss, judge a keep.
func (d *driver) verdict(ctx context.Context, c candidate) (outcome, error) {
	switch dec := c.res.Decision; {
	case dec.NoComparison:
		return noComparison, d.clean(ctx, c)
	case !dec.Keep:
		return d.revert(ctx, c, "decide: "+dec.Rule)
	}
	return d.judge(ctx, c)
}

// openLane is steps 3-4: a fresh lane and a before run in it.
func (d *driver) openLane(ctx context.Context, it Item) (candidate, error) {
	c := candidate{Item: it, slug: d.nextSlug(it.Pkg)}
	out, _, err := d.run(ctx, d.top, "", d.exe, "worktree", "create", c.slug)
	if err != nil {
		return c, err
	}
	c.lane = lastLine(out)
	c.work = filepath.Join(benchRoot(), "loop", c.slug)
	c.before = filepath.Join(c.work, "before-run.txt")
	if c.parent, err = source.GitInherit(c.lane, "rev-parse", "--short", "HEAD"); err == nil {
		err = measured(func() error {
			_, _, err := d.run(ctx, c.lane, "", d.exe, "review", "bench", "--path", it.Pkg, "--count", "8", "--out", c.before)
			return err
		})
	}
	if err != nil {
		return c, errors.Join(err, d.clean(ctx, c))
	}
	return c, nil
}

// nextSlug is bench-<pkg>-<yyyymmdd>-<n>, n the first whose lane branch does not exist.
func (d *driver) nextSlug(pkg string) string {
	day := time.Now().Format("20060102")
	for n := 1; ; n++ {
		slug := fmt.Sprintf("bench-%s-%s-%d", pkgDirName(pkg), day, n)
		if _, err := source.GitInherit(d.top, "rev-parse", "--verify", "--quiet", "refs/heads/worktree-"+slug); err != nil {
			return slug
		}
	}
}

// fixLeaf is step 5: the fix brief on a Databricks leaf working in the lane.
func (d *driver) fixLeaf(ctx context.Context, c *candidate) error {
	numbers, err := medians(c.before)
	if err != nil {
		return err
	}
	owned, err := ownedFiles(c.lane, c.Pkg)
	if err != nil {
		return err
	}
	pkg := "./" + c.Pkg
	brief, err := RenderBrief("fix", map[string]string{
		"OwnedLine": owned, "Verify": "rtk go test " + pkg, "Benchmark": "the benchmarks", "Package": pkg,
		"Workdir": c.lane, "Cause": c.What, "Numbers": numbers, "BeforeFile": c.before,
		"BenchCommand":    "rtk go test -run '^$' -bench . -benchmem -count 8 " + pkg,
		"IdentityCommand": "rtk go test -count=1 " + pkg,
	})
	if err != nil {
		return err
	}
	env, err := d.ask(ctx, c.lane, brief, "--provider", "databricks", "--tools")
	c.fixID = env.DispatchID
	return err
}

// gates is step 6's commit and step 7: a reason to revert, or "" when the fix passes.
func (d *driver) gates(ctx context.Context, c *candidate) (string, error) {
	head, err := source.GitInherit(c.lane, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	parent, err := source.GitInherit(c.lane, "rev-parse", "--verify", cmp.Or(c.parent, "HEAD")+"^{commit}")
	if err != nil {
		return "", err
	}
	if head != parent {
		return "gate: the fix leaf committed changes; hand back uncommitted", nil
	}
	files, err := laneStatusFiles(c.lane)
	if err != nil {
		return "", err
	}
	for _, file := range files {
		switch {
		case filepath.ToSlash(filepath.Dir(file)) != c.Pkg:
			return "gate: " + file + " is outside " + c.Pkg, nil
		case strings.HasSuffix(file, "_test.go"):
			return "gate: " + file + " is a test file", nil
		}
		c.files = append(c.files, file)
	}
	if len(c.files) == 0 {
		return "gate: the fix leaf changed nothing", nil
	}
	if err := d.commit(ctx, c); err != nil {
		return "", err
	}
	if _, _, err := d.run(ctx, c.lane, "", "go", "test", "./"+c.Pkg); err != nil {
		return "gate: go test ./" + c.Pkg + " failed", nil
	}
	return "", nil
}

func laneStatusFiles(lane string) ([]string, error) {
	status, err := source.GitInherit(lane, "status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var files []string
	entries := strings.Split(status, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		files = append(files, entry[3:])
		if strings.ContainsAny(entry[:2], "RC") {
			i++
			if i >= len(entries) || entries[i] == "" {
				return nil, errors.New("git status: missing rename source")
			}
			files = append(files, entries[i])
		}
	}
	return files, nil
}

// commit puts the fix on the lane branch, never main; the interleave refuses a dirty tree.
func (d *driver) commit(ctx context.Context, c *candidate) error {
	branch, err := source.GitInherit(c.lane, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != "worktree-"+c.slug {
		return errors.Join(fmt.Errorf("lane %s is on branch %q, not worktree-%s", c.lane, branch, c.slug), err)
	}
	msg := fmt.Sprintf("perf(%s): %s", filepath.Base(c.Pkg), c.What)
	if len(msg) > maxSubject {
		msg = strings.TrimSpace(msg[:maxSubject])
	}
	args := append([]string{"worktree", "commit", "-m", msg}, c.files...)
	if _, _, err := d.run(ctx, c.lane, "", d.exe, args...); err != nil {
		return err
	}
	c.fixSHA, err = source.GitInherit(c.lane, "rev-parse", "--short", "HEAD")
	return err
}

// interleave is step 6's A/B run: A a detached worktree at the parent, B the lane at the fix.
func (d *driver) interleave(ctx context.Context, c *candidate) error {
	return measured(func() error { return d.interleaveLocked(ctx, c) })
}

func (d *driver) interleaveLocked(ctx context.Context, c *candidate) error {
	parent := filepath.Join(c.work, "parent")
	if _, err := source.GitInherit(c.lane, "worktree", "add", "--detach", parent, c.parent); err != nil {
		return err
	}
	var err error
	c.res, err = Interleave(ctx, ABOptions{A: parent, B: c.lane, Pkg: c.Pkg, Rounds: abRounds, Count: abCount,
		Work: filepath.Join(c.work, "ab")}, d.log)
	if _, removeErr := source.GitInherit(c.lane, "worktree", "remove", "--force", parent); removeErr != nil {
		err = errors.Join(err, fmt.Errorf("%w: %s: %w", errParentWorktree, parent, removeErr))
	}
	return err
}

// judge is steps 9-12 after a deterministic KEEP: the Opus behaviour review, then keep, revert or hold.
func (d *driver) judge(ctx context.Context, c candidate) (outcome, error) {
	diff, err := source.GitInherit(c.lane, "diff", "--no-ext-diff", "--no-textconv", c.parent, "HEAD")
	if err != nil {
		return d.hold(c, "prepare review diff: "+err.Error())
	}
	owned, err := ownedFiles(c.lane, c.Pkg)
	if err != nil {
		return d.hold(c, "prepare review files: "+err.Error())
	}
	pkg := "./" + c.Pkg
	brief, err := RenderBrief("review", map[string]string{
		"OwnedLine": owned, "Verify": "rtk go test " + pkg, "Benchmark": "the benchmarks", "Package": pkg,
		"Workdir": c.lane, "Numbers": strings.Join(c.res.Decision.Rows, "\n"), "BeforeFile": c.res.Before,
		"AfterFile": c.res.After, "Diff": "```diff\n" + diff + "\n```", "IdentityCommand": "rtk go test -count=1 " + pkg,
	})
	if err != nil {
		return d.hold(c, "prepare review brief: "+err.Error())
	}
	env, err := d.ask(ctx, c.lane, brief, "--provider", "databricks", "--model", "opus", "--fallback", "none",
		"--read-only")
	verdict := strings.TrimSpace(firstLine(env.Answer))
	switch {
	case err != nil:
		return d.hold(c, "the Opus review failed: "+err.Error())
	case strings.Contains(env.Answer, ownerFlag):
		return d.hold(c, "the Opus review asks for the owner ("+ownerFlag+")")
	case verdict != "KEEP":
		return d.revert(ctx, c, "opus review: "+verdict)
	}
	return d.keep(c)
}

// keep is step 10-12 for a KEEP: the fix is already the lane commit; record it pending and post the notice.
func (d *driver) keep(c candidate) (outcome, error) {
	saved := savedMSDay(c)
	err := recordRow(loopOpts{record: true, verdict: "kept", pkg: c.Pkg, what: c.What, after: c.res.After,
		sha: c.fixSHA, lane: c.slug, dispatch: c.fixID, attempts: 1, savingMSDay: saved})
	if err != nil {
		return d.hold(c, "record keep: "+err.Error())
	}
	fmt.Fprintf(d.out, "bench: kept %s in lane %s, −%.0f ms/day (gates only); merge: claude-code worktree merge %s\n",
		c.What, c.slug, saved, c.slug)
	return kept, nil
}

// hold leaves the lane for the owner and records nothing, so a skip keeps this run off the item.
func (d *driver) hold(c candidate, why string) (outcome, error) {
	d.skip[closeKey(c.Pkg, c.What)] = true
	fmt.Fprintf(d.out, "bench: held %s in lane %s for the owner: %s; review: %s\n", c.What, c.slug, why, c.lane)
	return held, nil
}

// Record rejection before destroying the only recoverable lane state.
func (d *driver) revert(ctx context.Context, c candidate, why string) (outcome, error) {
	if err := recordRow(loopOpts{record: true, verdict: "reverted", pkg: c.Pkg, what: c.What,
		sha: cmp.Or(c.fixSHA, c.parent), dispatch: c.fixID, attempts: 1}); err != nil {
		return d.hold(c, "record revert: "+err.Error())
	}
	if err := d.clean(ctx, c); err != nil {
		return d.hold(c, "revert recorded but lane cleanup failed: "+err.Error())
	}
	fmt.Fprintf(d.out, "bench: reverted %s in lane %s: %s\n", c.What, c.slug, why)
	return reverted, nil
}

func (d *driver) clean(ctx context.Context, c candidate) error {
	_, _, err := d.run(ctx, d.top, "", d.exe, "worktree", "clean", "--force", c.slug)
	return err
}

// ask runs one background dispatch in dir, waits for it and returns its envelope.
func (d *driver) ask(ctx context.Context, dir, brief string, flags ...string) (askEnvelope, error) {
	var env askEnvelope
	args := append([]string{"agents", "ask", "--background", "--format", "json", "--workdir", dir}, flags...)
	out, _, err := d.run(ctx, d.top, brief, d.exe, append(args, "-")...)
	if err != nil {
		return env, err
	}
	var job struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(lastLine(out)), &job); err != nil || job.ID == "" {
		return env, fmt.Errorf("agents ask: no job id in %q", out)
	}
	env.DispatchID = job.ID
	if _, _, err := d.run(ctx, d.top, "", d.exe, "agents", "wait", job.ID, "--timeout", leafWait); err != nil {
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, _, cancelErr := d.run(cancelCtx, d.top, "", d.exe, "agents", "cancel", job.ID); cancelErr != nil {
			return env, errors.Join(fmt.Errorf("job %s: %w", job.ID, err), errJobActive, cancelErr)
		}
		return env, fmt.Errorf("job %s: %w", job.ID, err)
	}
	out, _, err = d.run(ctx, d.top, "", d.exe, "agents", "result", job.ID, "--format", "json")
	if err == nil {
		err = json.Unmarshal([]byte(out), &env)
	}
	env.DispatchID = cmp.Or(env.DispatchID, job.ID)
	if err == nil && env.Status != "ok" {
		err = fmt.Errorf("job %s: status %s", job.ID, env.Status)
	}
	return env, err
}

// run is one subprocess with list args; a failure carries the exit code and stderr.
func (d *driver) run(ctx context.Context, dir, stdin, name string, args ...string) (string, int, error) {
	return driverRun(ctx, dir, stdin, name, args...)
}

func execRun(ctx context.Context, dir, stdin, name string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outb, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outb, &errb
	if err := cmd.Run(); err != nil {
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		return outb.String(), code, fmt.Errorf("%s %s: %w\n%s", filepath.Base(name), strings.Join(args[:min(2, len(args))], " "),
			err, errb.String())
	}
	return outb.String(), 0, nil
}

// ownedFiles is the package's production Go files in the lane, as the JSON list a brief's Owned line takes.
func ownedFiles(lane, pkg string) (string, error) {
	files, err := filepath.Glob(filepath.Join(lane, pkg, "*.go"))
	if err != nil {
		return "", err
	}
	files = slices.DeleteFunc(files, func(f string) bool { return strings.HasSuffix(f, "_test.go") })
	b, err := json.Marshal(files)
	return string(b), err
}

// medians summarises a run file's benchmarks for a brief: median ns/op, B/op and allocs/op.
func medians(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, s := range scoreAll(string(b), false) {
		m := s.Metrics
		fmt.Fprintf(&sb, "%s: %.1f ns/op, %.0f B/op, %.0f allocs/op (%d samples)\n", s.Name, m["ns/op"].Median,
			m["B/op"].Median, m["allocs/op"].Median, s.Samples)
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("%s holds no benchmark samples", file)
	}
	return sb.String(), nil
}

// savedMSDay is calls/day times the largest median ns/op drop among the benchmarks the decision judged faster, in ms.
func savedMSDay(c candidate) float64 {
	bt, berr := os.ReadFile(c.res.Before)
	at, aerr := os.ReadFile(c.res.After)
	if berr != nil || aerr != nil {
		return 0
	}
	after := map[string]float64{}
	for _, s := range scoreAll(string(at), false) {
		after[s.Name] = s.Metrics["ns/op"].Median
	}
	best := 0.0
	for _, s := range scoreAll(string(bt), false) {
		a, ok := after[s.Name]
		if ok && slices.Contains(c.res.Decision.Faster, strings.TrimPrefix(s.Name, "Benchmark")) {
			best = max(best, s.Metrics["ns/op"].Median-a)
		}
	}
	return round(c.CallsPerDay * best / 1e6)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}
