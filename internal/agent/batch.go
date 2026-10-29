package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// DefaultPerKey caps concurrent tasks on one provider·profile (one credential, one rate limit).
const DefaultPerKey = 4

// DefaultJobs is the default pool size: min(4, GOMAXPROCS).
func DefaultJobs() int { return min(4, runtime.GOMAXPROCS(0)) }

// legLimiter caps concurrent running legs per billed account; a task holds at most one permit, never while queued.
type legLimiter struct {
	mu   sync.Mutex
	n    int
	sems map[string]chan struct{}
}

type legLimitKey struct{}

func withLegLimit(ctx context.Context, l *legLimiter) context.Context {
	return context.WithValue(ctx, legLimitKey{}, l)
}

// acquireLeg takes a permit on account under ctx's limiter; no limiter or account means no limit.
func acquireLeg(ctx context.Context, account string) (release func(), err error) {
	l, _ := ctx.Value(legLimitKey{}).(*legLimiter)
	if l == nil || account == "" {
		return func() {}, nil
	}
	l.mu.Lock()
	sem := l.sems[account]
	if sem == nil {
		sem = make(chan struct{}, l.n)
		l.sems[account] = sem
	}
	l.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

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

// BatchTask is one scheduled task. Prepare routes it and builds its dispatch record when it is about to start, so a
// cancelled queued task costs nothing; Err fails it before any run.
type BatchTask struct {
	ID      string
	Agent   string // the agent asked for, named on a failure envelope
	Req     Request
	Prepare func(ctx context.Context) (Chain, Record, error)
	Err     error
}

// BatchOptions configures RunBatch; Out is read when RunBatch is called, so a swapped os.Stdout is honored.
type BatchOptions struct {
	ID       string    // stamped on every envelope as batch_id
	Jobs     int       // tasks run at once; <= 0 means DefaultJobs
	FailFast bool      // the first failed task cancels every queued and running one
	Out      io.Writer // one envelope JSON per line, in completion order
	Log      io.Writer // per-task progress lines tagged with the task id; nil discards
}

// NewBatchID names one batch run.
func NewBatchID() string { return "batch-" + time.Now().UTC().Format("20060102T150405.000") }

type batchDone struct {
	i   int
	env Envelope
	ok  bool
}

// batchRun is the state shared by a batch's workers: the claimed record ids and the serialized log writer.
type batchRun struct {
	o    BatchOptions
	mu   sync.Mutex
	ids  map[string]bool
	logM sync.Mutex
	legs *legLimiter // one per batch, so accounts are capped across tasks
}

// RunBatch streams one envelope per task to o.Out as each finishes; true only if every task was ok.
// The calling goroutine is the one scheduler and the one writer: it starts the first queued task whose
// provider·profile has a free permit when a pool slot is free, so a full key never starves another.
func RunBatch(ctx context.Context, tasks []BatchTask, o BatchOptions) bool {
	jobs := max(cmp.Or(o.Jobs, DefaultJobs()), 1)
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b := &batchRun{o: o, ids: map[string]bool{}, legs: &legLimiter{n: DefaultPerKey, sems: map[string]chan struct{}{}}}
	allOK := true
	emit := func(env Envelope, ok bool) {
		env.BatchID = o.ID
		if !writeEnvelope(o.Out, env) {
			cancel() // nobody reads stdout any more: stop spending tokens
			ok = false
		}
		if !ok {
			allOK = false
			if o.FailFast && (env.Error == nil || env.Error.Kind != "canceled") {
				cancel()
			}
		}
	}
	var pending []int
	for i, t := range tasks {
		if t.Err != nil {
			emit(taskFailure(t, t.Err), false)
			continue
		}
		pending = append(pending, i)
	}
	keys := make([]string, len(tasks))
	for i, t := range tasks {
		keys[i] = cmp.Or(t.Req.Provider, t.Agent) // the profile billed, else the agent
	}
	sc := &batchSched{b: b, tasks: tasks, keys: keys, pending: pending, inKey: map[string]int{}, jobs: jobs,
		perKey: DefaultPerKey, done: make(chan batchDone, len(tasks)), emit: emit}
	for len(sc.pending) > 0 || sc.running > 0 {
		if bctx.Err() != nil {
			for _, i := range sc.pending {
				emit(b.cancelled(tasks[i]), false)
			}
			sc.pending = nil
		}
		sc.start(bctx)
		if sc.running == 0 {
			break // unreachable with perKey >= 1; never spin
		}
		sc.reap(bctx)
	}
	return allOK
}

// batchSched is the scheduler state RunBatch's single goroutine owns.
type batchSched struct {
	b            *batchRun
	tasks        []BatchTask
	keys         []string
	pending      []int
	inKey        map[string]int
	running      int
	jobs, perKey int
	done         chan batchDone
	emit         func(Envelope, bool)
}

// start launches queued tasks while a pool slot and a per-key permit are free.
func (sc *batchSched) start(ctx context.Context) {
	for sc.running < sc.jobs && len(sc.pending) > 0 && ctx.Err() == nil {
		at := freeSlot(sc.pending, sc.keys, sc.inKey, sc.perKey)
		if at < 0 {
			return
		}
		i := sc.pending[at]
		sc.pending = append(sc.pending[:at:at], sc.pending[at+1:]...)
		chain, rec, err := sc.tasks[i].Prepare(ctx)
		if err != nil {
			sc.emit(taskFailure(sc.tasks[i], err), false)
			continue
		}
		sc.inKey[sc.keys[i]]++
		sc.running++
		go func() { env, ok := sc.b.run(ctx, sc.tasks[i], chain, rec); sc.done <- batchDone{i, env, ok} }()
	}
}

// reap waits for one finished task and emits it; with queued work it also wakes on cancellation.
func (sc *batchSched) reap(ctx context.Context) {
	var d batchDone
	if len(sc.pending) > 0 {
		select {
		case d = <-sc.done:
		case <-ctx.Done():
			return
		}
	} else {
		d = <-sc.done
	}
	sc.running--
	sc.inKey[sc.keys[d.i]]--
	sc.emit(d.env, d.ok)
}

// writeEnvelope writes env as one JSON line; false only when the write itself failed.
func writeEnvelope(w io.Writer, env Envelope) bool {
	body, err := json.Marshal(env)
	if err != nil || w == nil {
		return true
	}
	_, werr := w.Write(append(body, '\n'))
	return werr == nil
}

// taskFailure is the error envelope of a task that never ran.
func taskFailure(t BatchTask, err error) Envelope {
	env := FailureEnvelope(t.Agent, err)
	env.TaskID = t.ID
	return env
}

// freeSlot is the index in pending of the first task whose key has a free permit, or -1.
func freeSlot(pending []int, keys []string, inKey map[string]int, perKey int) int {
	for p, i := range pending {
		if inKey[keys[i]] < perKey {
			return p
		}
	}
	return -1
}

// cancelled is the envelope of a queued task the batch cancelled before it started.
func (b *batchRun) cancelled(t BatchTask) Envelope {
	env := FailureEnvelope(t.Agent, fmt.Errorf("cancelled before start: %w", context.Canceled))
	env.TaskID = t.ID
	return env
}

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
func (b *batchRun) run(ctx context.Context, t BatchTask, chain Chain, rec Record) (env Envelope, ok bool) {
	ctx = withLegLimit(withIsolation(ctx), b.legs)
	start := time.Now()
	var sink *LogSink
	defer func() {
		if r := recover(); r != nil {
			if sink != nil {
				sink.Close() // idempotent: a panic after the normal close is harmless
			}
			err := &Error{Kind: KindFatal, Agent: t.Agent, Err: fmt.Errorf("panic: %v", r)}
			rec.Finish(Result{Agent: t.Agent, Status: StatusError}, err, time.Since(start))
			_ = rec.Save()
			env, ok = NewEnvelope(Result{Agent: t.Agent, Status: StatusError}, err, rec.ID), false
			env.TaskID = t.ID
		}
	}()
	b.reserve(&rec)
	_ = rec.Save() // fail-open: the log is for replay and forensics, never a gate on the run
	sink = NewLogSink(rec.LogPath())
	res, err := RunWithFallback(WithLog(ctx, sink), chain, t.Req, io.Discard, func(s string) { b.logf(t.ID, "%s", s) })
	sink.Close() // flush before the record leaves running, so tail -f never misses the last line
	for _, d := range res.Dropped {
		b.logf(t.ID, "%s has no channel on the %s agent — dropped", d, res.Agent)
	}
	rec.Finish(res, err, time.Since(start))
	if serr := rec.Save(); serr != nil {
		b.logf(t.ID, "dispatch %s record not saved, it stays at running: %v", rec.ID, serr)
	}
	b.logf(t.ID, "dispatch %s", rec.ID)
	env = NewEnvelope(res, err, rec.ID)
	env.TaskID = t.ID
	return env, err == nil && env.Status == StatusOK
}

// reserve gives rec an id no other task of this batch (or earlier record on disk) holds: ids are millisecond stamps.
func (b *batchRun) reserve(rec *Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	defaultLog := rec.LogPath() == filepath.Join(DispatchDir(), rec.ID+".log")
	for b.ids[rec.ID] || recordExists(rec.ID) {
		rec.Time = rec.Time.Add(time.Millisecond)
		rec.ID = recordID(rec.Time, rec.Agent)
	}
	b.ids[rec.ID] = true
	if defaultLog {
		rec.Log = filepath.Join(DispatchDir(), rec.ID+".log")
	}
}

func recordExists(id string) bool {
	_, err := os.Stat(filepath.Join(DispatchDir(), id+".json"))
	return err == nil
}
