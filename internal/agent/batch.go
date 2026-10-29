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

// DefaultPerKey caps concurrent legs on one billed account (one credential, one rate limit).
const DefaultPerKey = 4

// DefaultJobs is the default pool size: min(4, GOMAXPROCS).
func DefaultJobs() int { return min(4, runtime.GOMAXPROCS(0)) }

// limiter is the one concurrency gate for legs: at most jobs run at once, and at most cap per billed account.
// A waiting task holds nothing, so a full account never blocks another.
type limiter struct {
	mu        sync.Mutex
	cond      *sync.Cond
	running   int
	jobs, cap int
	per       map[string]int
	pending   int // prepared tasks that have not yet reached their first leg: each still owes a slot
}

// slot is one prepared task's claim on a global slot until its first leg asks for a permit (or parks, or ends).
type slot struct{ settled bool }

func newLimiter(jobs, cap int) *limiter {
	l := &limiter{jobs: jobs, cap: cap, per: map[string]int{}}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// wait blocks until ok() holds under the lock, or ctx is done; a cancelled ctx wins over a free slot.
func (l *limiter) wait(ctx context.Context, ok func() bool) error {
	defer context.AfterFunc(ctx, func() { l.mu.Lock(); l.cond.Broadcast(); l.mu.Unlock() })()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ok() {
			return nil
		}
		l.cond.Wait()
	}
}

// reserve waits, holding nothing, for a global slot no prepared task is owed, so a task is prepared only when it can
// start; the claim ends at settle.
func (l *limiter) reserve(ctx context.Context) (*slot, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.wait(ctx, func() bool { return l.running+l.pending < l.jobs }); err != nil {
		return nil, err
	}
	l.pending++
	return &slot{}, nil
}

func (l *limiter) settleLocked(s *slot) {
	if s != nil && !s.settled {
		s.settled = true
		l.pending--
		l.cond.Broadcast()
	}
}

// settle ends ctx's slot claim; idempotent, and a no-op outside a batch.
func settle(ctx context.Context) {
	if l, _ := ctx.Value(limiterKey{}).(*limiter); l != nil {
		s, _ := ctx.Value(slotKey{}).(*slot)
		l.mu.Lock()
		l.settleLocked(s)
		l.mu.Unlock()
	}
}

// acquire waits for a global slot and, when account is set, an account permit; the release func is idempotent.
func (l *limiter) acquire(ctx context.Context, account string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, _ := ctx.Value(slotKey{}).(*slot)
	l.settleLocked(s) // a parked task no longer owes a slot; an admitted one takes it in this same critical section
	if err := l.wait(ctx, func() bool { return l.running < l.jobs && (account == "" || l.per[account] < l.cap) }); err != nil {
		return nil, err
	}
	l.running++
	if account != "" {
		l.per[account]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.running--
			if account != "" {
				l.per[account]--
			}
			l.cond.Broadcast()
		})
	}, nil
}

// permit is a leg's current permit; Do swaps it out around retry waits so a backing-off leg holds nothing.
type permit struct {
	account string
	release func()
}

// idle runs wait with the permit released, then takes a permit again; the leg runs a child only while holding one.
func (h *permit) idle(ctx context.Context, wait func()) error {
	h.release()
	wait()
	r, err := acquireLeg(ctx, h.account)
	if err != nil {
		h.release = func() {}
		return err
	}
	h.release = r
	return nil
}

type (
	permitKey   struct{}
	limiterKey  struct{}
	failFastKey struct{}
	slotKey     struct{}
)

func withLimiter(ctx context.Context, l *limiter) context.Context {
	return context.WithValue(ctx, limiterKey{}, l)
}

// acquireLeg takes a leg permit under ctx's limiter; outside a batch there is no limit.
func acquireLeg(ctx context.Context, account string) (release func(), err error) {
	l, _ := ctx.Value(limiterKey{}).(*limiter)
	if l == nil {
		return func() {}, nil
	}
	return l.acquire(ctx, account)
}

// tripFailFast cancels the batch when err ends the task (no fallback follows), before the leg's permit is released.
func tripFailFast(ctx context.Context, c Chain, i int, req Request, res Result, err error) {
	trip, _ := ctx.Value(failFastKey{}).(func())
	if trip == nil || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if i+1 < len(c.Legs) && !(i == 0 && ExplicitBackend(req)) && legUnavailable(c.Legs[i].Agent, res, err) {
		return
	}
	trip()
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
	Jobs     int       // legs run at once; <= 0 means DefaultJobs
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

// batchRun is the state shared by a batch's workers: the claimed record ids, the serialized log writer, the limiter.
type batchRun struct {
	o    BatchOptions
	logM sync.Mutex
	lim  *limiter
	stop func() // cancels the batch
}

// RunBatch streams one envelope per task to o.Out as each finishes; true only if every task was ok.
// Each task runs in its own goroutine and waits on the limiter per leg; the calling goroutine is the one writer.
func RunBatch(ctx context.Context, tasks []BatchTask, o BatchOptions) bool {
	jobs := max(cmp.Or(o.Jobs, DefaultJobs()), 1)
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b := &batchRun{o: o, lim: newLimiter(jobs, DefaultPerKey), stop: cancel}
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

// feed prepares the queued tasks one at a time, in input order, each only once a global slot is free, and starts
// them; a cancelled batch prepares and starts nothing more.
func (b *batchRun) feed(ctx context.Context, tasks []BatchTask, queue []int, done chan<- batchDone) {
	for _, i := range queue {
		t := tasks[i]
		sl, err := b.lim.reserve(ctx)
		sctx := context.WithValue(withLimiter(ctx, b.lim), slotKey{}, sl) // settle needs the limiter to free the slot
		var chain Chain
		var rec Record
		if err == nil {
			chain, rec, err = t.Prepare(ctx)
		}
		switch {
		case err == nil:
			go func() { defer settle(sctx); env, ok := b.run(sctx, t, chain, rec); done <- batchDone{i, env, ok} }()
		case ctx.Err() != nil:
			settle(sctx)
			done <- batchDone{i, b.cancelled(t), false}
		default:
			settle(sctx)
			if b.o.FailFast {
				b.stop() // before the next task is considered, so it never starts
			}
			done <- batchDone{i, taskFailure(t, err), false}
		}
	}
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
	ctx = withLimiter(withIsolation(ctx), b.lim)
	if b.o.FailFast {
		ctx = context.WithValue(ctx, failFastKey{}, b.stop)
	}
	start := time.Now()
	var sink *LogSink
	defer func() { // runs after the recover below has set env: any failed task cancels before its slot is settled
		if !ok && b.o.FailFast && (env.Error == nil || env.Error.Kind != "canceled") {
			b.stop()
		}
	}()
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

// reserve gives rec an id no other task, batch or process holds: it links the complete "running" record to
// <id>.json (atomic, EEXIST if taken), bumping the millisecond stamp on a clash, so a claim is never an empty file.
func (b *batchRun) reserve(rec *Record) {
	dir := DispatchDir()
	defaultLog := rec.LogPath() == filepath.Join(dir, rec.ID+".log")
	if os.MkdirAll(dir, 0o700) != nil {
		return // fail-open, like Save: the log is for replay, never a gate on the run
	}
	for {
		if defaultLog {
			rec.Log = filepath.Join(dir, rec.ID+".log")
		}
		body, err := rec.encode()
		if err != nil {
			return
		}
		tmp, err := os.CreateTemp(dir, ".claim-*")
		if err != nil {
			return
		}
		_, werr := tmp.Write(body)
		cerr := tmp.Close()
		err = cmp.Or(werr, cerr)
		if err == nil {
			err = os.Link(tmp.Name(), filepath.Join(dir, rec.ID+".json"))
		}
		_ = os.Remove(tmp.Name())
		if !errors.Is(err, os.ErrExist) {
			return
		}
		rec.Time = rec.Time.Add(time.Millisecond)
		rec.ID = recordID(rec.Time, rec.Agent)
	}
}
