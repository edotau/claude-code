package ask

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"

	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// DefaultJobs is the default pool size: min(4, GOMAXPROCS).
func DefaultJobs() int { return min(4, runtime.GOMAXPROCS(0)) }

// Opts are the ask flags that apply to a whole --batch run; Identity is the --as value, empty when unset.
type Opts struct {
	Jobs            int
	FailFast        bool
	Agent, Fallback string
	Native          bool
	Identity        string
	FollowInput     bool
	FollowIdle      time.Duration
	RetryAgents     []string
	Decorate        func(*Task) // optional: called on every successfully built task
	// Follow optionally feeds more tasks after the initial lines; ctx is cancelled when RunBatch returns.
	Follow func(ctx context.Context, build func(ParsedLine) Task) <-chan Task
}

// BuildTask validates one line like the ask flags and attaches the routing step; routing waits for Prepare.
func BuildTask(l ParsedLine, o Opts, base agent.RunOptions, timeoutSet bool, cwd string, seen map[string]bool,
	newClient func() *client.Client) (Task, error) {
	t, err := ParseTask(l, o.Agent, base, timeoutSet, cwd, seen)
	if err != nil {
		return t, err
	}
	req := t.Req
	req.Agent, req.Fallback, req.Native = t.Agent, o.Fallback, o.Native
	cl := newClient()
	t.Prepare = func(ctx context.Context) (*client.Prepared, error) {
		p, err := cl.Prepare(ctx, req, nil)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", l.N, err)
		}
		return p, nil
	}
	return t, nil
}

// RunBatch builds a task per line (a bad line fails alone, logged to os.Stderr) and runs them on the pool; true only if all ok.
func RunBatch(ctx context.Context, lines []ParsedLine, o Opts, base agent.RunOptions, timeoutSet bool,
	newClient func() *client.Client, out io.Writer) (id string, ok bool) {
	ctx = providers.WithIdentity(ctx, cmp.Or(o.Identity, providers.IdentityHeadless))
	cwd, err := os.Getwd() // resolved once here: a pool goroutine must never fall back to the process cwd
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: ask: %v\n", err)
		return "", false
	}
	seen := map[string]bool{}
	build := func(l ParsedLine) Task {
		t, err := BuildTask(l, o, base, timeoutSet, cwd, seen, newClient)
		if err != nil {
			t.Err = err
			fmt.Fprintf(os.Stderr, "claude-code: ask: %v\n", err)
		} else if o.Decorate != nil {
			o.Decorate(&t)
		}
		return t
	}
	tasks := make([]Task, 0, len(lines))
	for _, l := range lines {
		tasks = append(tasks, build(l))
	}
	id = NewID()
	po := PoolOptions{ID: id, Jobs: o.Jobs, FailFast: o.FailFast, Out: out, Log: os.Stderr}
	if o.Follow != nil {
		fctx, cancel := context.WithCancel(ctx)
		defer cancel()
		po.More = o.Follow(fctx, build)
	}
	return id, RunTasks(ctx, tasks, po)
}

// PoolOptions configures RunTasks; Out is read when RunTasks is called, so a swapped os.Stdout is honored.
type PoolOptions struct {
	ID       string      // stamped on every envelope as batch_id
	Jobs     int         // dispatches run at once; <= 0 means DefaultJobs
	FailFast bool        // the first failed task cancels every queued and running one
	Out      io.Writer   // one envelope JSON per line, in completion order
	Log      io.Writer   // per-task progress lines tagged with the task id; nil discards
	More     <-chan Task // optional: tasks claimed as they arrive until it closes
}

type batchDone struct {
	i   int
	env wire.Envelope
	ok  bool
}

// batchRun is the state shared by a batch's workers: the serialized log writer, the limiter.
type batchRun struct {
	o    PoolOptions
	logM sync.Mutex
	lim  *retry.Limiter
	stop func() // cancels the batch
}

// RunTasks streams one envelope per task to o.Out as each finishes; true only if every task was ok.
func RunTasks(ctx context.Context, tasks []Task, o PoolOptions) bool {
	jobs := max(cmp.Or(o.Jobs, DefaultJobs()), 1)
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b := &batchRun{o: o, lim: retry.NewBatchLimiter(jobs), stop: cancel}
	allOK := true
	emit := func(env wire.Envelope, ok bool) {
		env.BatchID = o.ID
		if !writeEnvelope(o.Out, env) {
			cancel() // nobody reads stdout any more: stop spending tokens
			ok = false
		}
		if !ok {
			allOK = false
			if o.FailFast && cancelsBatch(env) {
				cancel()
			}
		}
	}
	done := make(chan batchDone, len(tasks))
	var queue []int
	for i, t := range tasks {
		if t.Err != nil {
			emit(taskFailure(t, t.Err), false)
			continue
		}
		queue = append(queue, i)
	}
	go b.feed(bctx, tasks, queue, done)
	for d := range done {
		emit(d.env, d.ok)
	}
	return allOK
}

// feed claims the initial queue in input order, then o.More; it closes done once every claimed task has finished.
func (b *batchRun) feed(ctx context.Context, tasks []Task, queue []int, done chan<- batchDone) {
	var wg sync.WaitGroup
	defer func() { wg.Wait(); close(done) }()
	for _, i := range queue {
		b.claim(ctx, &wg, tasks[i], i, done)
	}
	for b.o.More != nil {
		select {
		case t, ok := <-b.o.More:
			if !ok {
				return
			}
			if t.Err != nil {
				done <- batchDone{-1, taskFailure(t, t.Err), false}
				continue
			}
			b.claim(ctx, &wg, t, -1, done)
		case <-ctx.Done():
			return
		}
	}
}

// claim reserves a slot for t, then prepares and runs it in its own goroutine.
func (b *batchRun) claim(ctx context.Context, wg *sync.WaitGroup, t Task, i int, done chan<- batchDone) {
	sl, err := b.lim.Reserve(ctx)
	sctx := retry.WithBatchSlot(ctx, b.lim, sl) // settle needs the limiter to free the slot
	if err != nil {
		b.failBeforeStart(ctx, sctx, t, i, err, done, false)
		return
	}
	wg.Go(func() {
		b.execute(ctx, sctx, t, i, done)
	})
}

// execute prepares and runs t on its reserved slot, retrying a real failure once, and reports to done.
func (b *batchRun) execute(ctx, sctx context.Context, t Task, i int, done chan<- batchDone) {
	prepared, err := t.Prepare(ctx)
	if err != nil {
		b.failBeforeStart(ctx, sctx, t, i, err, done, true)
		return
	}
	defer retry.Settle(sctx)
	env, ok := b.run(sctx, t, prepared)
	if !ok {
		env = b.retry(sctx, t, env)
		ok = env.Status == wire.StatusOK && env.Error == nil
	}
	done <- batchDone{i, env, ok}
}

// failBeforeStart reports t as failed before it ran; stopFirst stops a FailFast batch before Settle frees the slot.
func (b *batchRun) failBeforeStart(ctx, sctx context.Context, t Task, i int, err error, done chan<- batchDone, stopFirst bool) {
	cancelled := ctx.Err() != nil
	if cancelled {
		err = fmt.Errorf("cancelled before start: %w", context.Canceled)
	}
	stop := func() {
		if b.o.FailFast && !cancelled {
			b.stop() // before the next task is considered, so it never starts
		}
	}
	if stopFirst {
		stop()
		retry.Settle(sctx)
	} else {
		retry.Settle(sctx)
		stop()
	}
	done <- batchDone{i, taskFailure(t, err), false}
}

// retry runs t.Retry once after a real failure and returns the retry's envelope with both attempts in Hops.
func (b *batchRun) retry(ctx context.Context, t Task, first wire.Envelope) wire.Envelope {
	if t.Retry == nil || ctx.Err() != nil || len(first.Hops) == 0 || !cancelsBatch(first) {
		return first
	}
	p, err := t.Retry(ctx)
	if err != nil {
		b.logf(t.ID, "retry not started: %v", err)
		return first
	}
	b.logf(t.ID, "retrying after %s", first.DispatchID)
	env, _ := b.run(ctx, t, p)
	env.Hops = append(slices.Clone(first.Hops), env.Hops...)
	return env
}

// writeEnvelope writes env as one JSON line; false only when the write itself failed.
func writeEnvelope(w io.Writer, env wire.Envelope) bool {
	body, err := json.Marshal(env)
	if err != nil || w == nil {
		return true
	}
	_, werr := w.Write(append(body, '\n'))
	return werr == nil
}

// taskFailure is the error envelope of a task that never ran.
func taskFailure(t Task, err error) wire.Envelope {
	env := wire.FailureEnvelope(t.Agent, err)
	env.TaskID = t.ID
	return env
}

// cancelsBatch reports whether a failed envelope should cancel a fail-fast batch: a cancellation itself does not.
func cancelsBatch(env wire.Envelope) bool { return env.Error == nil || env.Error.Kind != "canceled" }

// logf writes one progress line tagged with the task id.
func (b *batchRun) logf(id, format string, a ...any) {
	if b.o.Log == nil {
		return
	}
	b.logM.Lock()
	defer b.logM.Unlock()
	fmt.Fprintf(b.o.Log, "ask[%s]: %s\n", id, fmt.Sprintf(format, a...))
}

// run executes one task with its own stream, log sink and dispatch record; a panic becomes an error envelope.
func (b *batchRun) run(ctx context.Context, t Task, p *client.Prepared) (env wire.Envelope, ok bool) {
	rec := &p.Record
	rec.TaskID, rec.BatchID, rec.PlanTask = t.ID, b.o.ID, t.Plan
	ctx = agent.WithBatchRun(ctx, b.lim)
	if b.o.FailFast {
		ctx = retry.WithFailFast(ctx, b.stop)
	}
	start := time.Now()
	defer func() { // runs after the recover below has set env: any failed task cancels before its slot is settled
		if !ok && b.o.FailFast && cancelsBatch(env) {
			b.stop()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			err := retry.Fatal(t.Agent, fmt.Errorf("panic: %v", r))
			rec.Finish(wire.Result{Agent: t.Agent, Status: wire.StatusError}, err, time.Since(start))
			if serr := rec.Save(); serr != nil {
				b.logf(t.ID, "dispatch record %s not saved: %v", rec.ID, serr)
			}
			env, ok = wire.NewEnvelope(wire.Result{Agent: t.Agent, Status: wire.StatusError}, err, rec.ID), false
			env.TaskID = t.ID
		}
	}()
	if err := rec.Save(); err != nil { // fail-open: the log is for replay and forensics, never a gate on the run
		b.logf(t.ID, "dispatch record %s not saved: %v", rec.ID, err)
	}
	notify := func(s string) { b.logf(t.ID, "%s", s) }
	cl := client.NewClient(nil, nil, notify)
	if p.Opts.Launcher != nil {
		cl.Launcher = p.Opts.Launcher
	}
	res, err := cl.Exec(ctx, p, wire.Discard, job.RunRecordedOpts{})
	for _, d := range res.Dropped {
		b.logf(t.ID, "%s has no channel on the %s agent — dropped", d, res.Agent)
	}
	b.logf(t.ID, "dispatch %s", rec.ID)
	env = wire.NewEnvelope(res, err, res.DispatchID)
	env.TaskID = t.ID
	return env, err == nil && env.Status == wire.StatusOK
}

// batchExclusive are the ask flags that make no sense on a batch of independent tasks.
var batchExclusive = []string{"retry", "job", "background", "session", "log", "attach", "rubric", "debug-file"}

// runAskBatch runs the JSONL tasks in src (a file, or "-" for stdin) on a bounded pool and streams one envelope per line.
func runAskBatch(fs *flag.FlagSet, resume bool, src string, o Opts, base agent.RunOptions) int {
	if err := checkBatchFlags(fs, resume, o); err != nil {
		return failAskCode("text", o.Agent, err, 2)
	}
	return runAskBatchInput(fs, src, o, base)
}

func checkBatchFlags(fs *flag.FlagSet, resume bool, o Opts) error {
	var bad error
	if resume || fs.NArg() > 0 {
		bad = errors.New("--batch takes tasks from the JSONL source, not a session id or a positional task")
	}
	for _, f := range batchExclusive {
		if helptext.Explicit(fs, f) {
			bad = fmt.Errorf("--batch cannot be combined with --%s", f)
		}
	}
	if bad == nil {
		bad = checkBatchRun(fs, o)
	}
	return bad
}

// checkBatchRun rejects a non-JSON --format, --follow on stdin and a --jobs below 1.
func checkBatchRun(fs *flag.FlagSet, o Opts) error {
	if helptext.Explicit(fs, "format") && fs.Lookup("format").Value.String() != helptext.JSON {
		return errors.New("--batch always prints JSON envelopes; drop --format or pass --format json")
	}
	if o.FollowInput && fs.Lookup("batch").Value.String() == "-" {
		return errors.New("--follow needs a --batch file: stdin cannot be followed")
	}
	if o.Jobs < 1 {
		return fmt.Errorf("--jobs %d: need at least 1", o.Jobs)
	}
	return nil
}

func runAskBatchInput(fs *flag.FlagSet, src string, o Opts, base agent.RunOptions) int {
	in := io.Reader(os.Stdin)
	var fw *follower
	if o.FollowInput {
		var err error
		if fw, in, err = newFollower(src, o.FollowIdle); err != nil {
			return failAskCode("text", o.Agent, fmt.Errorf("--batch: %w", err), 2)
		}
	} else if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return failAskCode("text", o.Agent, fmt.Errorf("--batch: %w", err), 2)
		}
		defer f.Close()
		in = f
	}
	lines, err := ReadTaskLines(in)
	if err != nil {
		return failAskCode("text", o.Agent, fmt.Errorf("--batch: %w", err), 2)
	}
	if len(lines) == 0 && !o.FollowInput { // a followed file may get its tasks later
		return failAskCode("text", o.Agent, errors.New("--batch: no tasks in the input"), 2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGPIPE) // a closed stdout cancels
	defer stop()
	if as := fs.Lookup("as"); as != nil {
		o.Identity = as.Value.String()
	}
	o.Decorate = func(t *Task) { t.Retry = retryFor(*t, o, o.RetryAgents) }
	if fw != nil {
		o.Follow = fw.run
	}
	id, ok := RunBatch(ctx, lines, o, base, helptext.Explicit(fs, "timeout"), NewClient, os.Stdout)
	if !ok {
		fmt.Fprintf(os.Stderr, "ask: %s: not every task was ok\n", id)
		return 1
	}
	return 0
}
