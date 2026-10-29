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
// A waiting task holds nothing; waiters are served in ticket order among those whose account has room.
type limiter struct {
	mu        sync.Mutex
	cond      *sync.Cond
	running   int
	jobs, cap int
	per       map[string]int
	next      int
	wait      map[int]*waiter
}

// waiter is a queued ticket; account is empty until its holder reaches acquire (then it always has room).
type waiter struct{ account string }

func newLimiter(jobs, cap int) *limiter {
	l := &limiter{jobs: jobs, cap: cap, per: map[string]int{}, wait: map[int]*waiter{}}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// reserve queues a ticket now, so tasks are served in input order however their goroutines get scheduled.
func (l *limiter) reserve() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reserveLocked()
}

func (l *limiter) reserveLocked() int {
	l.next++
	l.wait[l.next] = &waiter{}
	return l.next
}

// drop gives up a ticket that never reached acquire.
func (l *limiter) drop(ticket int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.wait, ticket)
	l.cond.Broadcast()
}

func (l *limiter) room(account string) bool { return account == "" || l.per[account] < l.cap }

// first is the oldest queued ticket that could run if a global slot were free.
func (l *limiter) first() int {
	best := 0
	for t, w := range l.wait {
		if l.room(w.account) && (best == 0 || t < best) {
			best = t
		}
	}
	return best
}

// turn waits, holding nothing, until ticket is next in line with a global slot free: only then is the task prepared.
func (l *limiter) turn(ctx context.Context, ticket int) error {
	defer context.AfterFunc(ctx, func() { l.mu.Lock(); l.cond.Broadcast(); l.mu.Unlock() })()
	l.mu.Lock()
	defer l.mu.Unlock()
	for !(l.running < l.jobs && l.first() == ticket) {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.cond.Wait()
	}
	return ctx.Err()
}

// acquire waits for a global slot and, when account is set, an account permit; the release func is idempotent.
func (l *limiter) acquire(ctx context.Context, ticket int, account string) (func(), error) {
	defer context.AfterFunc(ctx, func() { l.mu.Lock(); l.cond.Broadcast(); l.mu.Unlock() })()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.wait[ticket]
	if w == nil {
		ticket = l.reserveLocked()
		w = l.wait[ticket]
	}
	w.account = account
	l.cond.Broadcast() // a full account no longer holds the head of the line
	for {
		if err := ctx.Err(); err != nil {
			delete(l.wait, ticket)
			l.cond.Broadcast()
			return nil, err
		}
		if l.running < l.jobs && l.room(account) && l.first() == ticket {
			break
		}
		l.cond.Wait()
	}
	delete(l.wait, ticket)
	l.running++
	if account != "" {
		l.per[account]++
	}
	l.cond.Broadcast() // the next waiter may fit too
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

// permitHold is the permit of the leg now running, released between legs and at RunWithFallback's exit.
type permitHold struct{ release func() }

func (h *permitHold) free() {
	if h.release != nil {
		h.release()
		h.release = nil
	}
}

// finish tells the batch the task's outcome (fail-fast cancels here) before the last permit is given back.
func (h *permitHold) finish(ctx context.Context, res Result, err error) {
	if f, _ := ctx.Value(finishKey{}).(func(Result, error)); f != nil {
		f(res, err)
	}
	h.free()
}

type (
	limiterKey struct{}
	finishKey  struct{}
	ticketKey  struct{}
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
	tk, _ := ctx.Value(ticketKey{}).(int)
	return l.acquire(ctx, tk, account)
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
	mu   sync.Mutex
	ids  map[string]bool
	logM sync.Mutex
	lim  *limiter
	stop func() // cancels the batch; fail-fast calls it from a finished task
}

// RunBatch streams one envelope per task to o.Out as each finishes; true only if every task was ok.
// Each task runs in its own goroutine and waits on the limiter per leg; the calling goroutine is the one writer.
func RunBatch(ctx context.Context, tasks []BatchTask, o BatchOptions) bool {
	jobs := max(cmp.Or(o.Jobs, DefaultJobs()), 1)
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b := &batchRun{o: o, ids: map[string]bool{}, lim: newLimiter(jobs, DefaultPerKey), stop: cancel}
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
	spawned := 0
	for i, t := range tasks {
		if t.Err != nil {
			emit(taskFailure(t, t.Err), false)
			continue
		}
		spawned++
		tk := b.lim.reserve()
		go func() {
			env, ok := b.task(context.WithValue(bctx, ticketKey{}, tk), t, tk)
			done <- batchDone{i, env, ok}
		}()
	}
	for range spawned {
		d := <-done
		emit(d.env, d.ok)
	}
	return allOK
}

// task prepares and runs one task; a cancelled batch fails it before Prepare, so a queued task costs nothing.
func (b *batchRun) task(ctx context.Context, t BatchTask, ticket int) (Envelope, bool) {
	defer b.lim.drop(ticket) // a ticket the task never used must not hold up the rest
	if b.lim.turn(ctx, ticket) != nil {
		return b.cancelled(t), false
	}
	chain, rec, err := t.Prepare(ctx)
	if err != nil {
		return taskFailure(t, err), false
	}
	return b.run(ctx, t, chain, rec)
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

// onFinish is the fail-fast trip: it runs while the task's last leg permit is still held.
func (b *batchRun) onFinish(res Result, err error) {
	if !b.o.FailFast {
		return
	}
	env := NewEnvelope(res, err, "")
	if env.Status != StatusOK && (env.Error == nil || env.Error.Kind != "canceled") {
		b.stop()
	}
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
	ctx = context.WithValue(withLimiter(withIsolation(ctx), b.lim), finishKey{}, b.onFinish)
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
