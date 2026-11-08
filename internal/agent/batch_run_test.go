package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	batch "github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// A line with trailing junk after its object is that line's error; Decoder.More alone accepts a stray '}'.
func TestReadTaskLinesRejectsTrailingJunk(t *testing.T) {
	in := "{\"prompt\":\"x\"}}\n{\"prompt\":\"a\"} {\"prompt\":\"b\"}\n{\"prompt\":\"ok\"}\n"
	got, err := batch.ReadTaskLines(strings.NewReader(in))
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d lines, err %v", len(got), err)
	}
	if got[0].Err == nil || got[1].Err == nil || got[2].Err != nil {
		t.Errorf("errs = %v / %v / %v, want error, error, nil", got[0].Err, got[1].Err, got[2].Err)
	}
}

type gaugeRunner struct {
	name     string
	cur, max *atomic.Int32
}

func (g gaugeRunner) Name() string     { return g.name }
func (g gaugeRunner) Caps() agent.Caps { return agent.Caps{} }

func (g gaugeRunner) Available() error { return nil }
func (g gaugeRunner) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	n := g.cur.Add(1)
	for m := g.max.Load(); n > m && !g.max.CompareAndSwap(m, n); m = g.max.Load() {
	}
	time.Sleep(60 * time.Millisecond)
	g.cur.Add(-1)
	return wire.Result{Answer: "a"}, nil
}

// Tasks queued under different agent names but dispatches billed to one account never run more than agent.DefaultPerKey at once.
func TestBatchCapsConcurrencyPerBilledAccount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var cur, peak atomic.Int32
	var tasks []batch.Task
	for i := range retry.DefaultPerKey + 3 {
		name := fmt.Sprintf("agent%d", i)
		r := gaugeRunner{name: name, cur: &cur, max: &peak}
		tasks = append(tasks, batch.Task{ID: name, Agent: name, Req: agent.RunOptions{Prompt: "p"},
			Prepare: func(context.Context) (*agent.Prepared, error) {
				return agent.SetChain(&agent.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: agent.Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}}, agent.Chain{Want: name, Dispatches: []agent.Dispatch{agent.KeepCaller(agent.Dispatch{Agent: r, Account: "shared·acct"})}}), nil
			}})
	}
	var out strings.Builder
	if !batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: len(tasks), Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > retry.DefaultPerKey || got < 2 {
		t.Errorf("peak concurrent dispatches on one account = %d, want 2..%d", got, retry.DefaultPerKey)
	}
}

type slowRunner struct {
	name  string
	sleep time.Duration
	boom  bool
	wait  <-chan struct{} // when set, Run blocks until it closes (or ctx ends)
	done  chan struct{}   // when set, Run closes it on entry
}

func (s slowRunner) Name() string     { return s.name }
func (s slowRunner) Caps() agent.Caps { return agent.Caps{} }

func (s slowRunner) Available() error { return nil }
func (s slowRunner) Run(ctx context.Context, _ agent.RunOptions, _ wire.Sink) (wire.Result, error) {
	if s.boom {
		panic("boom")
	}
	if s.done != nil {
		close(s.done)
	}
	if s.wait != nil {
		select {
		case <-s.wait:
		case <-ctx.Done():
			return wire.Result{}, ctx.Err()
		}
	}
	time.Sleep(s.sleep)
	return wire.Result{Answer: "a"}, nil
}

// acctTask is a batch task whose single dispatch bills account.
func acctTask(t *testing.T, name, account string, r agent.Engine) batch.Task {
	return batch.Task{ID: name, Agent: name, Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*agent.Prepared, error) {
			rec := agent.Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}
			return agent.SetChain(&agent.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: rec}, agent.Chain{Want: name, Dispatches: []agent.Dispatch{agent.KeepCaller(agent.Dispatch{Agent: r, Account: account})}}), nil
		}}
}

// A panicking agent (recovered by the batch) must give its dispatch permit back: later tasks on the account still run.
func TestBatchPanickingDispatchesReleaseTheirPermits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var tasks []batch.Task
	for i := range retry.DefaultPerKey + 2 {
		name := fmt.Sprintf("p%d", i)
		tasks = append(tasks, acctTask(t, name, "panic·acct", slowRunner{name: name, boom: true}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	if batch.RunTasks(ctx, tasks, batch.PoolOptions{Jobs: 2, Out: &out}) {
		t.Fatal("batch reported ok although every agent panicked")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(tasks) || strings.Contains(out.String(), "cancel") {
		t.Fatalf("got %d envelopes (want %d, none cancelled): a lost permit blocked the account\n%s", len(lines), len(tasks), out.String())
	}
	if n := strings.Count(out.String(), "panic: boom"); n != len(tasks) {
		t.Errorf("%d envelopes name the panic, want %d", n, len(tasks))
	}
}

// Tasks parked on a full account hold no pool slot: account-A tasks block until task b (idle account B) runs.
func TestBatchParkedTasksDoNotHoldPoolSlots(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	bRan := make(chan struct{})
	var tasks []batch.Task
	n := retry.DefaultPerKey + 2
	for i := range n {
		name := fmt.Sprintf("a%d", i)
		tasks = append(tasks, acctTask(t, name, "A·acct", slowRunner{name: name, wait: bRan}))
	}
	tasks = append(tasks, acctTask(t, "b", "B·acct", slowRunner{name: "b", done: bRan}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	if !batch.RunTasks(ctx, tasks, batch.PoolOptions{Jobs: n, Out: &out}) {
		t.Fatalf("batch failed: task b never got a pool slot while account-A tasks were parked\n%s", out.String())
	}
}

// dispatchGauge is an agent that fails (as auth) or answers after a pause, tracking one gauge shared by every dispatch.
type dispatchGauge struct {
	name     string
	fail     bool
	cur, max *atomic.Int32
}

func (g dispatchGauge) Name() string     { return g.name }
func (g dispatchGauge) Caps() agent.Caps { return agent.Caps{} }

func (g dispatchGauge) Available() error { return nil }
func (g dispatchGauge) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	n := g.cur.Add(1)
	for m := g.max.Load(); n > m && !g.max.CompareAndSwap(m, n); m = g.max.Load() {
	}
	time.Sleep(40 * time.Millisecond)
	g.cur.Add(-1)
	if g.fail {
		return wire.Result{}, errors.New("HTTP 401 unauthorized")
	}
	return wire.Result{Answer: "a"}, nil
}

// Dispatches in flight never exceed --jobs while tasks fall back from a full account A to account B.
func TestBatchJobsCapHoldsAcrossFallbackHops(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const jobs = 5
	var cur, peak atomic.Int32
	var tasks []batch.Task
	for i := range 12 {
		name := fmt.Sprintf("f%d", i)
		first, second := dispatchGauge{name: "codex", fail: true, cur: &cur, max: &peak}, dispatchGauge{name: "gemini", cur: &cur, max: &peak}
		tasks = append(tasks, batch.Task{ID: name, Agent: name, Req: agent.RunOptions{Prompt: "p"}, // distinct keys: the old per-key gate would cap at 4
			Prepare: func(context.Context) (*agent.Prepared, error) {
				rec := agent.Record{ID: "id-" + name, Agent: "codex", Log: filepath.Join(t.TempDir(), "l.log")}
				return agent.SetChain(&agent.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: rec}, agent.Chain{Want: "codex", Dispatches: []agent.Dispatch{
					agent.KeepCaller(agent.Dispatch{Agent: first, Account: "A·acct"}), {Agent: second, Account: "B·acct"}}}), nil
			}})
	}
	var out strings.Builder
	if !batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: jobs, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > jobs || got < 2 {
		t.Errorf("peak in-flight dispatches = %d, want 2..%d", got, jobs)
	}
	if n := strings.Count(out.String(), `"hops"`); n != len(tasks) {
		t.Errorf("%d envelopes carry hops, want %d (every task fell back)", n, len(tasks))
	}
}

// flagRunner counts its runs.
type flagRunner struct {
	name string
	runs *atomic.Int32
}

func (f flagRunner) Name() string     { return f.name }
func (f flagRunner) Caps() agent.Caps { return agent.Caps{} }

func (f flagRunner) Available() error { return nil }
func (f flagRunner) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	f.runs.Add(1)
	return wire.Result{Answer: "a"}, nil
}

// failFastPair runs first then a second task on Jobs=1 with FailFast; the second must neither prepare nor run.
func failFastPair(t *testing.T, first batch.Task) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var prepared, runs atomic.Int32
	second := acctTask(t, "second", "ff·acct", flagRunner{name: "second", runs: &runs})
	inner := second.Prepare
	second.Prepare = func(ctx context.Context) (*agent.Prepared, error) { prepared.Add(1); return inner(ctx) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out strings.Builder
	if batch.RunTasks(ctx, []batch.Task{first, second}, batch.PoolOptions{Jobs: 1, FailFast: true, Out: &out}) {
		t.Fatal("batch reported ok after a failed task")
	}
	if prepared.Load() != 0 || runs.Load() != 0 {
		t.Errorf("second task prepared %d times and ran %d times after --fail-fast tripped", prepared.Load(), runs.Load())
	}
	if !strings.Contains(out.String(), `"task_id":"second"`) || !strings.Contains(out.String(), "canceled") {
		t.Errorf("second task has no cancelled envelope:\n%s", out.String())
	}
}

// --fail-fast with a Prepare failure: the next task never spawns.
func TestBatchFailFastPrepareFailureStopsQueuedTask(t *testing.T) {
	failFastPair(t, batch.Task{ID: "first", Agent: "first", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*agent.Prepared, error) { return nil, errors.New("route failed") }})
}

// --fail-fast with a panicking dispatch: the next task never spawns.
func TestBatchFailFastPanickingDispatchStopsQueuedTask(t *testing.T) {
	failFastPair(t, acctTask(t, "first", "ff·acct", slowRunner{name: "first", boom: true}))
}

// A task waiting out its account's cooldown holds nothing: a task on another account runs and finishes first.
func TestBatchCoolingAccountDoesNotBlockOthers(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	agent.CoolDown("cool·A", 250*time.Millisecond)
	tasks := []batch.Task{
		acctTask(t, "cooling", "cool·A", slowRunner{name: "cooling"}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: 2, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if first, _, _ := strings.Cut(out.String(), "\n"); !strings.Contains(first, `"task_id":"free"`) {
		t.Errorf("first envelope = %s, want the free-account task", first)
	}
}

// Without --fail-fast a Prepare error frees its slot: with Jobs=1 the next task still runs.
func TestBatchPrepareFailureFreesSlotWithoutFailFast(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var runs atomic.Int32
	first := batch.Task{ID: "first", Agent: "first", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*agent.Prepared, error) { return nil, errors.New("route failed") }}
	second := acctTask(t, "second", "pf·acct", flagRunner{name: "second", runs: &runs})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	batch.RunTasks(ctx, []batch.Task{first, second}, batch.PoolOptions{Jobs: 1, Out: &out})
	if runs.Load() != 1 {
		t.Fatalf("second task ran %d times after the first task's Prepare failed, want 1:\n%s", runs.Load(), out.String())
	}
}

// A dispatch that panics after streaming output is terminal: the fallback dispatch never runs and repeats its work.
func TestBatchPanickingDispatchDoesNotFallBack(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var runs atomic.Int32
	task := batch.Task{ID: "t", Agent: "boom", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*agent.Prepared, error) {
			rec := agent.Record{ID: "id-t", Agent: "boom", Log: filepath.Join(t.TempDir(), "l.log")}
			return agent.SetChain(&agent.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: rec}, agent.Chain{Want: "boom", Dispatches: []agent.Dispatch{
				{Agent: slowRunner{name: "boom", boom: true}, Account: "boom·A"},
				{Agent: flagRunner{name: "next", runs: &runs}, Account: "next·B"},
			}}), nil
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	if batch.RunTasks(ctx, []batch.Task{task}, batch.PoolOptions{Jobs: 2, Out: &out}) {
		t.Fatal("batch reported ok after a panicking dispatch")
	}
	if runs.Load() != 0 || !strings.Contains(out.String(), "panic: boom") {
		t.Fatalf("fallback dispatch ran %d times after a panic, want 0:\n%s", runs.Load(), out.String())
	}
}

// A fallback dispatch that panics is still reported as the fallback, with the first dispatch's failure as the reason.
func TestBatchPanickingFallbackDispatchIsReported(t *testing.T) {
	agent.AuthEnv(t, `{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"AGAUTH_UNSET"},"models":{"opus":"m"}}`)
	first := agent.NewFailingRunner("claude", fmt.Errorf("agauth: %w", providers.ErrNeedsLogin))
	task := batch.Task{ID: "t", Agent: "claude", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*agent.Prepared, error) {
			rec := agent.Record{ID: "id-t", Agent: "claude", Log: filepath.Join(t.TempDir(), "l.log")}
			return agent.SetChain(&agent.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: rec}, agent.Chain{Want: "claude", Dispatches: []agent.Dispatch{
				{Agent: first, Account: "claude:agauth"},
				{Agent: slowRunner{name: "boom", boom: true}, Account: "boom·B"},
			}}), nil
		}}
	var out strings.Builder
	batch.RunTasks(context.Background(), []batch.Task{task}, batch.PoolOptions{Jobs: 2, Out: &out})
	var env wire.Envelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.Fallback == nil || env.FallbackReason == "" || len(env.Hops) != 2 {
		t.Fatalf("fallback=%v reason=%q hops=%d, want the panicking dispatch reported as the fallback", env.Fallback, env.FallbackReason, len(env.Hops))
	}
}

// --fail-fast: a task that fails outside any dispatch (an empty chain) still cancels before the next task is prepared.
func TestBatchFailFastEmptyChainStopsQueuedTask(t *testing.T) {
	failFastPair(t, batch.Task{ID: "first", Agent: "first", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*agent.Prepared, error) {
			return agent.SetChain(&agent.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: agent.Record{ID: "id-first", Agent: "first", Log: filepath.Join(t.TempDir(), "l.log")}}, agent.Chain{Want: "first"}), nil
		}})
}

type rateOnce struct {
	name string
	n    *atomic.Int32
}

func (r rateOnce) Name() string     { return r.name }
func (r rateOnce) Caps() agent.Caps { return agent.Caps{} }

func (r rateOnce) Available() error { return nil }
func (r rateOnce) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	if r.n.Add(1) == 1 {
		return wire.Result{}, &retry.Error{Kind: retry.KindRateLimit, Agent: r.name, RetryAfter: 300 * time.Millisecond, Err: errors.New("HTTP 429")}
	}
	return wire.Result{Answer: "a"}, nil
}

// A dispatch backing off after a 429 holds no permit: with Jobs=1 a task on another account runs during the wait.
func TestBatchBackoffReleasesDispatchPermit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var n atomic.Int32
	tasks := []batch.Task{
		acctTask(t, "limited", "rate·A", rateOnce{name: "limited", n: &n}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: 1, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if first, _, _ := strings.Cut(out.String(), "\n"); !strings.Contains(first, `"task_id":"free"`) {
		t.Errorf("first envelope = %s, want the free-account task to finish during the backoff", first)
	}
}

// A cooldown that starts between the dispatch's acquire and its first attempt is waited out without the permit.
func TestBatchFirstAttemptCooldownReleasesDispatchPermit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var once sync.Once
	t.Cleanup(agent.SwapLoadState(func(prev func() router.State) func() router.State {
		return func() router.State {
			once.Do(func() { agent.CoolDown("late·A", 200*time.Millisecond) })
			return prev()
		}
	}))
	tasks := []batch.Task{
		acctTask(t, "late", "late·A", slowRunner{name: "late"}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: 1, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if first, _, _ := strings.Cut(out.String(), "\n"); !strings.Contains(first, `"task_id":"free"`) {
		t.Errorf("first envelope = %s, want the free-account task to run during the cooldown", first)
	}
}

// A dispatch whose account another task marked auth-dead while it waited for a permit does not run: it is an auth hop.
func TestQueuedDispatchSkipsAccountMarkedAuthDead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var runs atomic.Int32
	first := markingRunner{name: "first", mark: func() { _ = router.MarkAuth("auth·A", "login expired") }}
	tasks := []batch.Task{
		acctTask(t, "first", "auth·A", first),
		acctTask(t, "second", "auth·A", flagRunner{name: "second", runs: &runs}),
	}
	var out strings.Builder
	batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: 1, Out: &out})
	if runs.Load() != 0 {
		t.Errorf("the queued dispatch ran %d times on an auth-dead account", runs.Load())
	}
	for ln := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(ln, `"task_id":"second"`) && !strings.Contains(ln, `"kind":"auth"`) {
			t.Errorf("second task's envelope = %s, want an auth error", ln)
		}
	}
}

type markingRunner struct {
	name string
	mark func()
}

func (m markingRunner) Name() string     { return m.name }
func (m markingRunner) Caps() agent.Caps { return agent.Caps{} }

func (m markingRunner) Available() error { return nil }
func (m markingRunner) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	m.mark()
	time.Sleep(50 * time.Millisecond)
	return wire.Result{Answer: "a"}, nil
}

// A retry resumes after the backoff only if its account is still usable: one marked auth-dead meanwhile is skipped.
func TestRetryRechecksAccountAfterBackoff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var n atomic.Int32
	tasks := []batch.Task{
		acctTask(t, "limited", "rate·A", rateOnce{name: "limited", n: &n}),
		acctTask(t, "marker", "free·B", markingRunner{name: "marker", mark: func() { _ = router.MarkAuth("rate·A", "login expired") }}),
	}
	var out strings.Builder
	batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: 1, Out: &out})
	if n.Load() != 1 {
		t.Errorf("the rate-limited dispatch ran %d times, want 1: its retry must not run on an auth-dead account", n.Load())
	}
	for ln := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(ln, `"task_id":"limited"`) && !strings.Contains(ln, `"kind":"auth"`) {
			t.Errorf("limited task's envelope = %s, want an auth error", ln)
		}
	}
}

type cancelAfterRun struct {
	name   string
	cancel context.CancelFunc
}

func (c cancelAfterRun) Name() string     { return c.name }
func (c cancelAfterRun) Caps() agent.Caps { return agent.Caps{} }

func (c cancelAfterRun) Available() error { return nil }
func (c cancelAfterRun) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	c.cancel() // the parent ctx dies at the moment the dispatch succeeds
	return wire.Result{Answer: "a"}, nil
}

// A cooldown that idles the dispatch before attempt 1 is followed by a recheck: an account marked auth-dead meanwhile
// is not attempted.
func TestFirstAttemptRechecksAccountAfterCooldownIdle(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var once sync.Once
	t.Cleanup(agent.SwapLoadState(func(prev func() router.State) func() router.State {
		return func() router.State {
			once.Do(func() { agent.CoolDown("late·A", 200*time.Millisecond) })
			return prev()
		}
	}))
	var runs atomic.Int32
	tasks := []batch.Task{
		acctTask(t, "late", "late·A", flagRunner{name: "late", runs: &runs}),
		acctTask(t, "marker", "free·B", markingRunner{name: "marker", mark: func() { _ = router.MarkAuth("late·A", "login expired") }}),
	}
	var out strings.Builder
	batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: 1, Out: &out})
	if runs.Load() != 0 {
		t.Errorf("the first attempt ran %d times on an account marked auth-dead during its cooldown wait", runs.Load())
	}
}

// Own-login dispatches (no account) on one agent still share agent.DefaultPerKey, even when --jobs allows more.
func TestBatchCapsOwnLoginDispatchesPerAgent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var cur, peak atomic.Int32
	r := gaugeRunner{name: "opencode", cur: &cur, max: &peak}
	var tasks []batch.Task
	for i := range retry.DefaultPerKey + 3 {
		id := fmt.Sprintf("own%d", i)
		tasks = append(tasks, batch.Task{ID: id, Agent: "opencode", Req: agent.RunOptions{Prompt: "p"},
			Prepare: func(context.Context) (*agent.Prepared, error) {
				return agent.SetChain(&agent.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: agent.Record{ID: "id-" + id, Agent: "opencode", Log: filepath.Join(t.TempDir(), "l.log")}}, agent.Chain{Want: "opencode", Dispatches: []agent.Dispatch{agent.KeepCaller(agent.Dispatch{Agent: r})}}), nil
			}})
	}
	var out strings.Builder
	if !batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: len(tasks), Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > retry.DefaultPerKey || got < 2 {
		t.Errorf("peak concurrent own-login dispatches = %d, want 2..%d", got, retry.DefaultPerKey)
	}
}

// batch.RunTasks writes one shared marker per running dispatch and removes every marker when the batch is done.
func TestRunBatchMarkersAppearAndClear(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const account = "shared·acct"
	var cur, runPeak atomic.Int32
	var tasks []batch.Task
	for _, name := range []string{"a", "b", "c"} {
		r := gaugeRunner{name: name, cur: &cur, max: &runPeak}
		tasks = append(tasks, batch.Task{ID: name, Agent: name, Req: agent.RunOptions{Prompt: "p"},
			Prepare: func(context.Context) (*agent.Prepared, error) {
				return agent.SetChain(&agent.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: agent.Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}}, agent.Chain{Want: name, Dispatches: []agent.Dispatch{agent.KeepCaller(agent.Dispatch{Agent: r, Account: account})}}), nil
			}})
	}
	stop := make(chan struct{})
	var peak atomic.Int32
	go agent.SamplePeak(inflight.Dir(), account, 2*time.Millisecond, stop, &peak)

	if !batch.RunTasks(context.Background(), tasks, batch.PoolOptions{Jobs: 3}) {
		t.Fatal("batch failed")
	}
	close(stop)
	if got := peak.Load(); got == 0 {
		t.Error("no inflight markers observed while the batch ran")
	}
	if n := agent.CountAccountMarkers(inflight.Dir(), account); n != 0 {
		t.Errorf("%d markers remain after batch.RunTasks returned, want 0", n)
	}
}

// A cancel that lands right after a dispatch succeeded must not overwrite its answer.
func TestSuccessSurvivesCancelRightAfterIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	c := agent.Chain{Want: "first", Dispatches: []agent.Dispatch{{Agent: cancelAfterRun{name: "first", cancel: cancel}}, {Agent: flagRunner{name: "second", runs: &runs}}}}
	res, err := agent.RunWithFallback(ctx, c, agent.RunOptions{Prompt: "p"}, wire.Discard, nil)
	if err != nil || res.Answer != "a" || runs.Load() != 0 || len(res.Hops) != 1 {
		t.Fatalf("res=%+v err=%v second ran %d times; want the first dispatch's answer alone", res, err, runs.Load())
	}
}
