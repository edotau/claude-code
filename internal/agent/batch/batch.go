package batch

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// DefaultJobs is the default pool size: min(4, GOMAXPROCS).
func DefaultJobs() int { return min(4, runtime.GOMAXPROCS(0)) }

// DefaultToolsTimeout is the deadline of a fresh --tools run without an explicit timeout.
const DefaultToolsTimeout = 45 * time.Minute

// TaskLine is one `ask --batch` JSONL task; Tools stays nil when absent so the flag default applies.
type TaskLine struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Profile string `json:"profile"`
	Model   string `json:"model"`
	Prompt  string `json:"prompt"`
	Workdir string `json:"workdir"`
	Tools   *bool  `json:"tools"`
	Timeout string `json:"timeout"` // a Go duration such as 5m
}

// ParsedLine is one non-blank input line: its 1-based number, the decoded task, or why it did not decode.
type ParsedLine struct {
	N    int
	Task TaskLine
	Err  error
}

// ReadTaskLines decodes JSONL tasks; a malformed line or unknown field is that line's Err, never the whole batch's.
func ReadTaskLines(r io.Reader) ([]ParsedLine, error) {
	var out []ParsedLine
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			pl := ParsedLine{N: n}
			dec := json.NewDecoder(bytes.NewReader(line))
			dec.DisallowUnknownFields()
			if derr := dec.Decode(&pl.Task); derr != nil {
				pl.Err = fmt.Errorf("line %d: %w", n, derr)
			} else if extra := (json.RawMessage)(nil); !errors.Is(dec.Decode(&extra), io.EOF) {
				pl.Err = fmt.Errorf("line %d: trailing data after the JSON object", n) // More() misses a stray '}'
			}
			out = append(out, pl)
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// Task is one scheduled task; Prepare routes it when it is about to start, and Err fails it before any run.
type Task struct {
	ID      string
	Agent   string // the agent asked for, named on a failure envelope
	Req     agent.RunOptions
	Prepare func(ctx context.Context) (*agent.Prepared, error)
	Err     error
}

// PoolOptions configures RunTasks; Out is read when RunTasks is called, so a swapped os.Stdout is honored.
type PoolOptions struct {
	ID       string    // stamped on every envelope as batch_id
	Jobs     int       // dispatches run at once; <= 0 means DefaultJobs
	FailFast bool      // the first failed task cancels every queued and running one
	Out      io.Writer // one envelope JSON per line, in completion order
	Log      io.Writer // per-task progress lines tagged with the task id; nil discards
}

// NewID names one batch run.
func NewID() string {
	var b [3]byte
	_, _ = rand.Read(b[:]) // a failed read leaves zeros: the stamp alone still names the batch
	return "batch-" + batchNow().UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(b[:])
}

// batchNow is the clock NewID reads; a var so tests can force two batches onto one millisecond.
var batchNow = time.Now

type batchDone struct {
	i   int
	env agent.Envelope
	ok  bool
}

// batchRun is the state shared by a batch's workers: the serialized log writer, the limiter.
type batchRun struct {
	o    PoolOptions
	logM sync.Mutex
	lim  *agent.Limiter
	stop func() // cancels the batch
}

// RunTasks streams one envelope per task to o.Out as each finishes; true only if every task was ok.
func RunTasks(ctx context.Context, tasks []Task, o PoolOptions) bool {
	jobs := max(cmp.Or(o.Jobs, DefaultJobs()), 1)
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b := &batchRun{o: o, lim: agent.NewBatchLimiter(jobs), stop: cancel}
	allOK := true
	emit := func(env agent.Envelope, ok bool) {
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
	for range queue {
		d := <-done
		emit(d.env, d.ok)
	}
	return allOK
}

// feed prepares queued tasks in input order, each once a slot is free; a cancelled batch starts nothing more.
func (b *batchRun) feed(ctx context.Context, tasks []Task, queue []int, done chan<- batchDone) {
	for _, i := range queue {
		t := tasks[i]
		sl, err := b.lim.Reserve(ctx)
		sctx := agent.WithBatchSlot(ctx, b.lim, sl) // settle needs the limiter to free the slot
		var prepared *agent.Prepared
		if err == nil {
			prepared, err = t.Prepare(ctx)
		}
		switch err {
		case nil:
			go func() { defer agent.Settle(sctx); env, ok := b.run(sctx, t, prepared); done <- batchDone{i, env, ok} }()
		default:
			cancelled := ctx.Err() != nil
			if cancelled {
				err = fmt.Errorf("cancelled before start: %w", context.Canceled)
			}
			agent.Settle(sctx)
			if b.o.FailFast && !cancelled {
				b.stop() // before the next task is considered, so it never starts
			}
			done <- batchDone{i, taskFailure(t, err), false}
		}
	}
}

// writeEnvelope writes env as one JSON line; false only when the write itself failed.
func writeEnvelope(w io.Writer, env agent.Envelope) bool {
	body, err := json.Marshal(env)
	if err != nil || w == nil {
		return true
	}
	_, werr := w.Write(append(body, '\n'))
	return werr == nil
}

// taskFailure is the error envelope of a task that never ran.
func taskFailure(t Task, err error) agent.Envelope {
	env := agent.FailureEnvelope(t.Agent, err)
	env.TaskID = t.ID
	return env
}

// cancelsBatch reports whether a failed envelope should cancel a fail-fast batch: a cancellation itself does not.
func cancelsBatch(env agent.Envelope) bool { return env.Error == nil || env.Error.Kind != "canceled" }

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
func (b *batchRun) run(ctx context.Context, t Task, p *agent.Prepared) (env agent.Envelope, ok bool) {
	rec := &p.Record
	ctx = agent.WithBatchRun(ctx, b.lim)
	if b.o.FailFast {
		ctx = agent.WithFailFast(ctx, b.stop)
	}
	start := time.Now()
	defer func() { // runs after the recover below has set env: any failed task cancels before its slot is settled
		if !ok && b.o.FailFast && cancelsBatch(env) {
			b.stop()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			err := &agent.Error{Kind: agent.KindFatal, Agent: t.Agent, Err: fmt.Errorf("panic: %v", r)}
			rec.Finish(agent.Result{Agent: t.Agent, Status: agent.StatusError}, err, time.Since(start))
			_ = rec.Save()
			env, ok = agent.NewEnvelope(agent.Result{Agent: t.Agent, Status: agent.StatusError}, err, rec.ID), false
			env.TaskID = t.ID
		}
	}()
	_ = rec.Save() // fail-open: the log is for replay and forensics, never a gate on the run
	notify := func(s string) { b.logf(t.ID, "%s", s) }
	client := agent.NewClient(nil, nil, notify)
	if p.Opts.Launcher != nil {
		client.Launcher = p.Opts.Launcher
	}
	res, err := client.Exec(ctx, p, agent.Discard, agent.RunRecordedOpts{})
	for _, d := range res.Dropped {
		b.logf(t.ID, "%s has no channel on the %s agent — dropped", d, res.Agent)
	}
	b.logf(t.ID, "dispatch %s", rec.ID)
	env = agent.NewEnvelope(res, err, res.DispatchID)
	env.TaskID = t.ID
	return env, err == nil && env.Status == agent.StatusOK
}

var batchIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ParseTask validates one line like the ask flags; on error the task keeps its id and agent.
func ParseTask(l ParsedLine, agentName string, base agent.RunOptions, timeoutSet bool, cwd string,
	seen map[string]bool) (Task, error) {
	t := Task{ID: cmp.Or(l.Task.ID, "t"+strconv.Itoa(l.N)), Agent: cmp.Or(l.Task.Agent, agentName)}
	if l.Err != nil {
		return t, l.Err
	}
	tl := l.Task
	if !batchIDRE.MatchString(t.ID) {
		return t, fmt.Errorf("line %d: id %q must match %s", l.N, t.ID, batchIDRE)
	}
	if seen[t.ID] {
		return t, fmt.Errorf("line %d: duplicate id %q", l.N, t.ID)
	}
	seen[t.ID] = true
	if strings.TrimSpace(tl.Prompt) == "" {
		return t, fmt.Errorf("line %d: no task: give a prompt", l.N)
	}
	req := base
	req.Prompt, req.Provider, req.Model = tl.Prompt, cmp.Or(tl.Profile, base.Provider), cmp.Or(tl.Model, base.Model)
	name, err := agent.SelectAndOverlay(t.Agent, base.Agents, &req) // explicit line/base options win over the definition
	if err != nil {
		return t, fmt.Errorf("line %d: %w", l.N, err)
	}
	t.Agent = name
	if tl.Tools != nil {
		req.Tools = *tl.Tools
	}
	if req.Tools && req.NoTools {
		return t, fmt.Errorf("line %d: tools and --no-tools are mutually exclusive", l.N)
	}
	req, err = batchTaskLocation(l, req, timeoutSet, base.WorkDir, cwd)
	if err != nil {
		return t, err
	}
	t.Req = req
	return t, nil
}

func batchTaskLocation(l ParsedLine, req agent.RunOptions, timeoutSet bool, baseWorkdir, cwd string) (agent.RunOptions, error) {
	switch {
	case l.Task.Timeout != "":
		d, err := time.ParseDuration(l.Task.Timeout)
		if err != nil || d <= 0 {
			return req, fmt.Errorf("line %d: timeout %q must be a positive duration such as 5m", l.N, l.Task.Timeout)
		}
		req.Timeout = d
	case req.Tools && !timeoutSet:
		req.Timeout = DefaultToolsTimeout
	}
	req.WorkDir = cmp.Or(l.Task.Workdir, baseWorkdir, cwd)
	abs, err := filepath.Abs(req.WorkDir)
	if err != nil {
		return req, fmt.Errorf("line %d: workdir %q: %w", l.N, req.WorkDir, err)
	}
	req.WorkDir = abs
	return req, nil
}
