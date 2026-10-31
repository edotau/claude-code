package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func hmJWT(iss string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"https://`+iss+`"}`)) + ".sig"
}

// hmProfile overlays a Databricks profile "hm" on host adb-1 whose credential comes from auth.command cmd.
func hmProfile(t *testing.T, cmd string) { hmNamed(t, "hm", cmd) }

// hmNamed is hmProfile under name; command credentials are cached per name for the process, so reruns need fresh ones.
func hmNamed(t *testing.T, name, cmd string) {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"`+name+`":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","command":`+fmt.Sprintf("%q", cmd)+`},`+
		`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5","haiku":"databricks-claude-haiku-4-5"}}}}}`)
}

// A line with trailing junk after its object is that line's error; Decoder.More alone accepts a stray '}'.
func TestReadTaskLinesRejectsTrailingJunk(t *testing.T) {
	in := "{\"prompt\":\"x\"}}\n{\"prompt\":\"a\"} {\"prompt\":\"b\"}\n{\"prompt\":\"ok\"}\n"
	got, err := ReadTaskLines(strings.NewReader(in))
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d lines, err %v", len(got), err)
	}
	if got[0].Err == nil || got[1].Err == nil || got[2].Err != nil {
		t.Errorf("errs = %v / %v / %v, want error, error, nil", got[0].Err, got[1].Err, got[2].Err)
	}
}

// A waiter whose cooldown is extended by another task while it sleeps must keep waiting for the new deadline.
func TestWaitCooldownHonorsExtendedDeadline(t *testing.T) {
	key := "cool-extend-" + t.Name()
	coolDown(key, 100*time.Millisecond)
	start := time.Now()
	done := make(chan time.Duration)
	go func() { waitCooldown(context.Background(), key, 5*time.Second); done <- time.Since(start) }()
	time.Sleep(40 * time.Millisecond)
	coolDown(key, 300*time.Millisecond)
	if got := <-done; got < 300*time.Millisecond {
		t.Errorf("waiter resumed after %v, want it to wait out the extended ~340ms hold", got)
	}
	cooldowns.Lock()
	delete(cooldowns.until, key)
	cooldowns.Unlock()
}

type gaugeRunner struct {
	name     string
	cur, max *atomic.Int32
}

func (g gaugeRunner) Name() string     { return g.name }
func (g gaugeRunner) Caps() Caps       { return Caps{} }
func (g gaugeRunner) Available() error { return nil }
func (g gaugeRunner) Run(context.Context, RunOptions, Sink) (Result, error) {
	n := g.cur.Add(1)
	for m := g.max.Load(); n > m && !g.max.CompareAndSwap(m, n); m = g.max.Load() {
	}
	time.Sleep(60 * time.Millisecond)
	g.cur.Add(-1)
	return Result{Answer: "a"}, nil
}

// Tasks queued under different agent names but dispatches billed to one account never run more than DefaultPerKey at once.
func TestBatchCapsConcurrencyPerBilledAccount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var cur, peak atomic.Int32
	var tasks []BatchTask
	for i := range DefaultPerKey + 3 {
		name := fmt.Sprintf("agent%d", i)
		r := gaugeRunner{name: name, cur: &cur, max: &peak}
		tasks = append(tasks, BatchTask{ID: name, Agent: name, Req: RunOptions{Prompt: "p"},
			Prepare: func(context.Context) (*Prepared, error) {
				return &Prepared{Opts: RunOptions{Prompt: "p"},
					chain:  Chain{Want: name, Dispatches: []Dispatch{{Agent: r, Account: "shared·acct", keepCaller: true}}},
					Record: Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}}, nil
			}})
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: len(tasks), Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > DefaultPerKey || got < 2 {
		t.Errorf("peak concurrent dispatches on one account = %d, want 2..%d", got, DefaultPerKey)
	}
}

// A routed provider:model request on a vendor agent is guarded as that provider, not skipped as an own login.
func TestGuardProviderResolvesModelPrefixForVendorAgent(t *testing.T) {
	hmProfile(t, "true")
	p, err := guardProvider(&fakeRunner{name: "codex"}, RunOptions{Model: "hm:opus"})
	if err != nil || p == nil || p.Name != "hm" {
		t.Fatalf("guardProvider = %v, %v; want the hm profile", p, err)
	}
}

// The host mismatch of a prefix-routed vendor request fails as auth and the runner never starts.
func TestRunPrefixRoutedHostMismatchFailsAuth(t *testing.T) {
	hmProfile(t, "true")
	t.Setenv("HM_KEY", hmJWT("adb-2.azuredatabricks.net"))
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"hm":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","env":"HM_KEY"},"models":{"opus":"m"}}}}}`)
	r := &fakeRunner{name: "codex", caps: Caps{Model: Native}, errs: []error{nil}}
	_, err := Run(withIsolation(context.Background()), r, RunOptions{Prompt: "p", Model: "hm:opus"}, Discard, nil)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindAuth || len(r.seen) != 0 {
		t.Fatalf("err = %v, runs = %d; want KindAuth before any run", err, len(r.seen))
	}
}

// A transient credential-command failure is retried inside Run instead of failing the hop as auth.
func TestRunTransientCredentialFailureIsRetried(t *testing.T) {
	fastDefaultBackoff(t)
	flag := filepath.Join(t.TempDir(), "seen")
	name := fmt.Sprintf("hmt%d", time.Now().UnixNano())
	hmNamed(t, name, fmt.Sprintf("if [ -f %[1]s ]; then echo %[2]s; else touch %[1]s; echo 'connection reset' >&2; exit 1; fi", flag, hmJWT("adb-1.azuredatabricks.net")))
	r := &fakeRunner{name: "claude", caps: Caps{Provider: Native}, errs: []error{nil}}
	res, err := Run(context.Background(), r, RunOptions{Prompt: "p", Provider: name}, Discard, nil)
	if err != nil || res.Answer != "a" || res.Transport.Attempts != 2 {
		t.Fatalf("res=%+v err=%v; want an answer on attempt 2", res, err)
	}
}

// An isolated copilot SDK run blanks other profiles' credential env, since the SDK merges os.Environ itself.
func TestCopilotSDKEnvBlanksForeignCredentials(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	pinGitHubProfile(t, "alice", `"anthropic":{"other":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"HM_OTHER_KEY"},"models":{"opus":"m"}}}`)
	t.Setenv("HM_OTHER_KEY", "foreign-secret")
	var mu sync.Mutex
	var seen []string
	prev := copilotAsk
	copilotAsk = func(_ context.Context, o copilotsdk.Options, _ string, _ []string) (copilotsdk.Result, error) {
		mu.Lock()
		seen = slices.Clone(o.Env)
		mu.Unlock()
		return copilotsdk.Result{Text: "hi", SessionID: "s"}, nil
	}
	t.Cleanup(func() { copilotAsk = prev })
	if _, err := Run(withIsolation(context.Background()), copilotAgent, RunOptions{Prompt: "p", Provider: "alice", Direct: true}, Discard, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(seen, "HM_OTHER_KEY=") || slices.Contains(seen, "HM_OTHER_KEY=foreign-secret") {
		t.Errorf("sdk env = %v, want HM_OTHER_KEY blanked", seen)
	}
}

type slowRunner struct {
	name  string
	sleep time.Duration
	boom  bool
}

func (s slowRunner) Name() string     { return s.name }
func (s slowRunner) Caps() Caps       { return Caps{} }
func (s slowRunner) Available() error { return nil }
func (s slowRunner) Run(context.Context, RunOptions, Sink) (Result, error) {
	if s.boom {
		panic("boom")
	}
	time.Sleep(s.sleep)
	return Result{Answer: "a"}, nil
}

// acctTask is a batch task whose single dispatch bills account.
func acctTask(t *testing.T, name, account string, r Runner) BatchTask {
	return BatchTask{ID: name, Agent: name, Req: RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*Prepared, error) {
			rec := Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}
			return &Prepared{Opts: RunOptions{Prompt: "p"}, Record: rec,
				chain: Chain{Want: name, Dispatches: []Dispatch{{Agent: r, Account: account, keepCaller: true}}}}, nil
		}}
}

// A panicking runner (recovered by the batch) must give its dispatch permit back: later tasks on the account still run.
func TestBatchPanickingDispatchesReleaseTheirPermits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var tasks []BatchTask
	for i := range DefaultPerKey + 2 {
		name := fmt.Sprintf("p%d", i)
		tasks = append(tasks, acctTask(t, name, "panic·acct", slowRunner{name: name, boom: true}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	if RunBatch(ctx, tasks, BatchOptions{Jobs: 2, Out: &out}) {
		t.Fatal("batch reported ok although every runner panicked")
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != len(tasks) || strings.Contains(out.String(), "cancel") {
		t.Fatalf("got %d envelopes (want %d, none cancelled): a lost permit blocked the account\n%s", len(lines), len(tasks), out.String())
	}
	if n := strings.Count(out.String(), "panic: boom"); n != len(tasks) {
		t.Errorf("%d envelopes name the panic, want %d", n, len(tasks))
	}
}

// Tasks parked on a full account hold no pool slot: a task on an idle account finishes while they still wait.
func TestBatchParkedTasksDoNotHoldPoolSlots(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var tasks []BatchTask
	n := DefaultPerKey + 2
	for i := range n {
		name := fmt.Sprintf("a%d", i)
		tasks = append(tasks, acctTask(t, name, "A·acct", slowRunner{name: name, sleep: 150 * time.Millisecond}))
	}
	tasks = append(tasks, acctTask(t, "b", "B·acct", slowRunner{name: "b"}))
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: n, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if first, _, _ := strings.Cut(out.String(), "\n"); !strings.Contains(first, `"task_id":"b"`) {
		t.Errorf("first envelope = %s, want task b: it queued behind parked account-A tasks", first)
	}
}

// An auth.command whose JWT was issued for another host fails the hop as auth, and fallback moves to the next hop.
func TestCommandCredentialHostMismatchIsAuthAndFallsBack(t *testing.T) {
	name := fmt.Sprintf("hmm%d", time.Now().UnixNano())
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"`+name+`","providers":{"databricks":{"`+name+`":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","command":"echo `+hmJWT("adb-2.azuredatabricks.net")+`"},`+
		`"models":{"opus":"m"}}}}}`)
	t.Cleanup(func() { vaulttest.Put(t, vault.Overlay, "providers", `{}`) }) // the default must not leak to later tests
	prev := markAuth
	markAuth = func(string, string) error { return nil }
	t.Cleanup(func() { markAuth = prev })
	first := &fakeRunner{name: "claude", caps: Caps{Provider: Native}, errs: []error{nil}}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	c := Chain{Want: "claude", Dispatches: []Dispatch{{Agent: first, Account: "claude:" + name, keepCaller: true}, {Agent: second}}}
	res, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, Discard, nil)
	if err != nil || res.Answer != "a" || len(first.seen) != 0 || len(second.seen) != 1 {
		t.Fatalf("res=%+v err=%v first=%d second=%d; want hop 2 to answer", res, err, len(first.seen), len(second.seen))
	}
	if len(res.Hops) != 2 || res.Hops[0].ErrorKind != "auth" {
		t.Errorf("hops = %+v, want hop 1 error_kind auth", res.Hops)
	}
}

// dispatchGauge is a runner that fails (as auth) or answers after a pause, tracking one gauge shared by every dispatch.
type dispatchGauge struct {
	name     string
	fail     bool
	cur, max *atomic.Int32
}

func (g dispatchGauge) Name() string     { return g.name }
func (g dispatchGauge) Caps() Caps       { return Caps{} }
func (g dispatchGauge) Available() error { return nil }
func (g dispatchGauge) Run(context.Context, RunOptions, Sink) (Result, error) {
	n := g.cur.Add(1)
	for m := g.max.Load(); n > m && !g.max.CompareAndSwap(m, n); m = g.max.Load() {
	}
	time.Sleep(40 * time.Millisecond)
	g.cur.Add(-1)
	if g.fail {
		return Result{}, errors.New("HTTP 401 unauthorized")
	}
	return Result{Answer: "a"}, nil
}

// Dispatches in flight never exceed --jobs while tasks fall back from a full account A to account B.
func TestBatchJobsCapHoldsAcrossFallbackHops(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const jobs = 5
	var cur, peak atomic.Int32
	var tasks []BatchTask
	for i := range 12 {
		name := fmt.Sprintf("f%d", i)
		first, second := dispatchGauge{name: "codex", fail: true, cur: &cur, max: &peak}, dispatchGauge{name: "gemini", cur: &cur, max: &peak}
		tasks = append(tasks, BatchTask{ID: name, Agent: name, Req: RunOptions{Prompt: "p"}, // distinct keys: the old per-key gate would cap at 4
			Prepare: func(context.Context) (*Prepared, error) {
				rec := Record{ID: "id-" + name, Agent: "codex", Log: filepath.Join(t.TempDir(), "l.log")}
				return &Prepared{Opts: RunOptions{Prompt: "p"}, Record: rec, chain: Chain{Want: "codex", Dispatches: []Dispatch{
					{Agent: first, Account: "A·acct", keepCaller: true}, {Agent: second, Account: "B·acct"}}}}, nil
			}})
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: jobs, Out: &out}) {
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
func (f flagRunner) Caps() Caps       { return Caps{} }
func (f flagRunner) Available() error { return nil }
func (f flagRunner) Run(context.Context, RunOptions, Sink) (Result, error) {
	f.runs.Add(1)
	return Result{Answer: "a"}, nil
}

// failFastPair runs first then a second task on Jobs=1 with FailFast; the second must neither prepare nor run.
func failFastPair(t *testing.T, first BatchTask) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var prepared, runs atomic.Int32
	second := acctTask(t, "second", "ff·acct", flagRunner{name: "second", runs: &runs})
	inner := second.Prepare
	second.Prepare = func(ctx context.Context) (*Prepared, error) { prepared.Add(1); return inner(ctx) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out strings.Builder
	if RunBatch(ctx, []BatchTask{first, second}, BatchOptions{Jobs: 1, FailFast: true, Out: &out}) {
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
	failFastPair(t, BatchTask{ID: "first", Agent: "first", Req: RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*Prepared, error) { return nil, errors.New("route failed") }})
}

// --fail-fast with a panicking dispatch: the next task never spawns.
func TestBatchFailFastPanickingDispatchStopsQueuedTask(t *testing.T) {
	failFastPair(t, acctTask(t, "first", "ff·acct", slowRunner{name: "first", boom: true}))
}

// A task waiting out its account's cooldown holds nothing: a task on another account runs and finishes first.
func TestBatchCoolingAccountDoesNotBlockOthers(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	coolDown("cool·A", 250*time.Millisecond)
	tasks := []BatchTask{
		acctTask(t, "cooling", "cool·A", slowRunner{name: "cooling"}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: 2, Out: &out}) {
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
	first := BatchTask{ID: "first", Agent: "first", Req: RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*Prepared, error) { return nil, errors.New("route failed") }}
	second := acctTask(t, "second", "pf·acct", flagRunner{name: "second", runs: &runs})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	RunBatch(ctx, []BatchTask{first, second}, BatchOptions{Jobs: 1, Out: &out})
	if runs.Load() != 1 {
		t.Fatalf("second task ran %d times after the first task's Prepare failed, want 1:\n%s", runs.Load(), out.String())
	}
}

// A dispatch that panics after streaming output is terminal: the fallback dispatch never runs and repeats its work.
func TestBatchPanickingDispatchDoesNotFallBack(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var runs atomic.Int32
	task := BatchTask{ID: "t", Agent: "boom", Req: RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*Prepared, error) {
			rec := Record{ID: "id-t", Agent: "boom", Log: filepath.Join(t.TempDir(), "l.log")}
			return &Prepared{Opts: RunOptions{Prompt: "p"}, Record: rec, chain: Chain{Want: "boom", Dispatches: []Dispatch{
				{Agent: slowRunner{name: "boom", boom: true}, Account: "boom·A"},
				{Agent: flagRunner{name: "next", runs: &runs}, Account: "next·B"},
			}}}, nil
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	if RunBatch(ctx, []BatchTask{task}, BatchOptions{Jobs: 2, Out: &out}) {
		t.Fatal("batch reported ok after a panicking dispatch")
	}
	if runs.Load() != 0 || !strings.Contains(out.String(), "panic: boom") {
		t.Fatalf("fallback dispatch ran %d times after a panic, want 0:\n%s", runs.Load(), out.String())
	}
}

// A fallback dispatch that panics is still reported as the fallback, with the first dispatch's failure as the reason.
func TestBatchPanickingFallbackDispatchIsReported(t *testing.T) {
	authEnv(t, `{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"AGAUTH_UNSET"},"models":{"opus":"m"}}`)
	first := &fakeRunner{name: "claude", errs: []error{fmt.Errorf("agauth: %w", providers.ErrNeedsLogin)}, emptyAnswer: true}
	task := BatchTask{ID: "t", Agent: "claude", Req: RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*Prepared, error) {
			rec := Record{ID: "id-t", Agent: "claude", Log: filepath.Join(t.TempDir(), "l.log")}
			return &Prepared{Opts: RunOptions{Prompt: "p"}, Record: rec, chain: Chain{Want: "claude", Dispatches: []Dispatch{
				{Agent: first, Account: "claude:agauth"},
				{Agent: slowRunner{name: "boom", boom: true}, Account: "boom·B"},
			}}}, nil
		}}
	var out strings.Builder
	RunBatch(context.Background(), []BatchTask{task}, BatchOptions{Jobs: 2, Out: &out})
	var env Envelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.Fallback == nil || env.FallbackReason == "" || len(env.Hops) != 2 {
		t.Fatalf("fallback=%v reason=%q hops=%d, want the panicking dispatch reported as the fallback", env.Fallback, env.FallbackReason, len(env.Hops))
	}
}

// Under batch isolation an own-login child never sees another profile's custom credential var, but keeps the
// native var its launch sets; outside a batch nothing is scrubbed.
func TestChildCmdOwnLoginDropsForeignCredentialsUnderIsolation(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"pa":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"PA_BATCH_KEY"},"models":{"opus":"m"}},`+
		`"pb":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OPENCODE_API_KEY"},"models":{"opus":"m"}}}}}`)
	t.Setenv("PA_BATCH_KEY", "foreign-secret")
	has := func(cmd interface{ Environ() []string }, kv string) bool { return slices.Contains(cmd.Environ(), kv) }
	iso, _ := childCmd(withIsolation(context.Background()), "/bin/true", nil, []string{"OPENCODE_API_KEY=native"}, "", "", nil, true)
	if has(iso, "PA_BATCH_KEY=foreign-secret") || !has(iso, "OPENCODE_API_KEY=native") {
		t.Errorf("isolated env leaks PA_BATCH_KEY or lost the native var: %v", iso.Env)
	}
	plain, _ := childCmd(context.Background(), "/bin/true", nil, nil, "", "", nil, true)
	if !has(plain, "PA_BATCH_KEY=foreign-secret") {
		t.Error("outside a batch the env must stay unchanged")
	}
}

// Two batches (separate processes in effect) stamping the same millisecond never share a record id or file.
func TestReserveIsExclusiveAcrossBatches(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			ids[i] = ClaimRecord("codex", RunOptions{Prompt: "p"}, now).ID // the one allocator every dispatch path uses
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("record id %s handed out twice: %v", id, ids)
		}
		seen[id] = true
		if _, err := os.Stat(filepath.Join(DispatchDir(), id+".json")); err != nil {
			t.Errorf("record %s has no claimed file: %v", id, err)
		}
	}
}

// --fail-fast: a task that fails outside any dispatch (an empty chain) still cancels before the next task is prepared.
func TestBatchFailFastEmptyChainStopsQueuedTask(t *testing.T) {
	failFastPair(t, BatchTask{ID: "first", Agent: "first", Req: RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*Prepared, error) {
			return &Prepared{Opts: RunOptions{Prompt: "p"}, chain: Chain{Want: "first"},
				Record: Record{ID: "id-first", Agent: "first", Log: filepath.Join(t.TempDir(), "l.log")}}, nil
		}})
}

type rateOnce struct {
	name string
	n    *atomic.Int32
}

func (r rateOnce) Name() string     { return r.name }
func (r rateOnce) Caps() Caps       { return Caps{} }
func (r rateOnce) Available() error { return nil }
func (r rateOnce) Run(context.Context, RunOptions, Sink) (Result, error) {
	if r.n.Add(1) == 1 {
		return Result{}, &Error{Kind: KindRateLimit, Agent: r.name, RetryAfter: 300 * time.Millisecond, Err: errors.New("HTTP 429")}
	}
	return Result{Answer: "a"}, nil
}

// A dispatch backing off after a 429 holds no permit: with Jobs=1 a task on another account runs during the wait.
func TestBatchBackoffReleasesDispatchPermit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var n atomic.Int32
	tasks := []BatchTask{
		acctTask(t, "limited", "rate·A", rateOnce{name: "limited", n: &n}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: 1, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if first, _, _ := strings.Cut(out.String(), "\n"); !strings.Contains(first, `"task_id":"free"`) {
		t.Errorf("first envelope = %s, want the free-account task to finish during the backoff", first)
	}
}

// A hub dispatch keeps the credential its launch supplies even when another profile declares the same env name; the
// inherited value of that name is gone.
func TestChildCmdLaunchValuesSurviveIsolationScrub(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"ocgo":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OCGO_KEY"},"models":{"opus":"m"}},`+
		`"other":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OPENCODE_API_KEY"},"models":{"opus":"m"}}}}}`)
	t.Setenv("OPENCODE_API_KEY", "inherited-foreign")
	reg, err := providers.Load()
	if err != nil || reg.Providers["ocgo"] == nil {
		t.Fatalf("registry: %v", err)
	}
	ctx := withOwnProvider(withIsolation(context.Background()), reg.Providers["ocgo"])
	cmd, _ := childCmd(ctx, "/bin/true", nil, []string{"OPENCODE_API_KEY=launch"}, "", "", nil, true)
	if !slices.Contains(cmd.Env, "OPENCODE_API_KEY=launch") || slices.Contains(cmd.Env, "OPENCODE_API_KEY=inherited-foreign") {
		t.Errorf("env keeps the inherited foreign value or lost the launch's: %v", slices.DeleteFunc(slices.Clone(cmd.Env),
			func(kv string) bool { return !strings.HasPrefix(kv, "OPENCODE_API_KEY") }))
	}
}

// A reserved record is a complete running record from its first byte, so a concurrent prune keeps it.
func TestReservedRecordSurvivesPrune(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := dispatchKeep
	dispatchKeep = 1
	t.Cleanup(func() { dispatchKeep = old })
	dir := DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	done := Record{Schema: DispatchSchema, ID: "99991231T000000.000-zz", Agent: "zz", Status: "ok"}
	if err := done.Save(); err != nil {
		t.Fatal(err)
	}
	rec := ClaimRecord("aa", RunOptions{Prompt: "p"}, time.Now())
	pruneDispatches(dir) // lands between reserve and Save
	if _, err := os.Stat(filepath.Join(dir, rec.ID+".json")); err != nil {
		t.Errorf("the reserved record was pruned before Save: %v", err)
	}
}

// A cooldown that starts between the dispatch's acquire and its first attempt is waited out without the permit.
func TestBatchFirstAttemptCooldownReleasesDispatchPermit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var once sync.Once
	prev := loadState
	loadState = func() router.State {
		once.Do(func() { coolDown("late·A", 200*time.Millisecond) })
		return prev()
	}
	t.Cleanup(func() { loadState = prev })
	tasks := []BatchTask{
		acctTask(t, "late", "late·A", slowRunner{name: "late"}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: 1, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if first, _, _ := strings.Cut(out.String(), "\n"); !strings.Contains(first, `"task_id":"free"`) {
		t.Errorf("first envelope = %s, want the free-account task to run during the cooldown", first)
	}
}

// A chat stream that ends without [DONE] is a dropped connection: it retries, and the second attempt answers.
func TestChatStreamWithoutDoneIsRetried(t *testing.T) {
	fastDefaultBackoff(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		if calls == 1 {
			return // an empty 200: the connection just ends
		}
		for _, chunk := range []string{`{"model":"gpt-x","choices":[{"delta":{"content":"hi"}}]}`, `[DONE]`} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	defer srv.Close()
	registry(t, srv)
	res, err := Run(context.Background(), claudeAgent, RunOptions{Prompt: "hi", Provider: "toai", NoTools: true}, Discard, nil)
	if err != nil || res.Answer != "hi" || res.Transport == nil || res.Transport.Attempts != 2 {
		t.Fatalf("res=%+v err=%v calls=%d; want an answer on attempt 2", res, err, calls)
	}
}

// A dispatch whose account another task marked auth-dead while it waited for a permit does not run: it is an auth hop.
func TestQueuedDispatchSkipsAccountMarkedAuthDead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var runs atomic.Int32
	first := markingRunner{name: "first", mark: func() { _ = router.MarkAuth("auth·A", "login expired") }}
	tasks := []BatchTask{
		acctTask(t, "first", "auth·A", first),
		acctTask(t, "second", "auth·A", flagRunner{name: "second", runs: &runs}),
	}
	var out strings.Builder
	RunBatch(context.Background(), tasks, BatchOptions{Jobs: 1, Out: &out})
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
func (m markingRunner) Caps() Caps       { return Caps{} }
func (m markingRunner) Available() error { return nil }
func (m markingRunner) Run(context.Context, RunOptions, Sink) (Result, error) {
	m.mark()
	time.Sleep(50 * time.Millisecond)
	return Result{Answer: "a"}, nil
}

// A 429 followed by success is one hop, no error_kind, whose errors list the recovered rate_limit attempt.
func TestHopKeepsRecoveredAttemptErrors(t *testing.T) {
	fastDefaultBackoff(t)
	r := &fakeRunner{name: "claude", errs: []error{&Error{Kind: KindRateLimit, Agent: "claude", Err: errors.New("HTTP 429")}, nil}}
	res, err := RunWithFallback(context.Background(), Single(r), RunOptions{Prompt: "p"}, Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := envelopeMap(t, res, err)
	hops, _ := env["hops"].([]any)
	if len(hops) != 1 {
		t.Fatalf("hops = %v, want one", env["hops"])
	}
	h, _ := hops[0].(map[string]any)
	errs, _ := h["errors"].([]any)
	if h["error_kind"] != nil || len(errs) != 1 || errs[0].(map[string]any)["kind"] != "rate_limit" {
		t.Errorf("hop = %v, want no error_kind and one rate_limit error", h)
	}
}

// The final attempt's 429 still publishes its cooldown: a second Do on the same key waits for it.
func TestDoPublishesCooldownOnFinalAttempt(t *testing.T) {
	key := "final-" + t.Name()
	p := Policy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Second, Key: key}
	Do(context.Background(), p, func(context.Context, int) *Error {
		return &Error{Kind: KindRateLimit, Agent: "a", RetryAfter: 300 * time.Millisecond, Err: errors.New("429")}
	})
	start := time.Now()
	Do(context.Background(), p, func(context.Context, int) *Error { return nil })
	if got := time.Since(start); got < 200*time.Millisecond {
		t.Errorf("second Do started after %v, want it held for the published ~300ms cooldown", got)
	}
}

// A retry resumes after the backoff only if its account is still usable: one marked auth-dead meanwhile is skipped.
func TestRetryRechecksAccountAfterBackoff(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var n atomic.Int32
	tasks := []BatchTask{
		acctTask(t, "limited", "rate·A", rateOnce{name: "limited", n: &n}),
		acctTask(t, "marker", "free·B", markingRunner{name: "marker", mark: func() { _ = router.MarkAuth("rate·A", "login expired") }}),
	}
	var out strings.Builder
	RunBatch(context.Background(), tasks, BatchOptions{Jobs: 1, Out: &out})
	if n.Load() != 1 {
		t.Errorf("the rate-limited dispatch ran %d times, want 1: its retry must not run on an auth-dead account", n.Load())
	}
	for ln := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(ln, `"task_id":"limited"`) && !strings.Contains(ln, `"kind":"auth"`) {
			t.Errorf("limited task's envelope = %s, want an auth error", ln)
		}
	}
}

// A transient credential failure that a retry recovers from is listed in the answering hop's errors.
func TestHopKeepsRecoveredGuardFailure(t *testing.T) {
	fastDefaultBackoff(t)
	flag := filepath.Join(t.TempDir(), "seen")
	name := fmt.Sprintf("hmg%d", time.Now().UnixNano())
	hmNamed(t, name, fmt.Sprintf("if [ -f %[1]s ]; then echo %[2]s; else touch %[1]s; echo 'connection reset' >&2; exit 1; fi", flag, hmJWT("adb-1.azuredatabricks.net")))
	r := &fakeRunner{name: "claude", caps: Caps{Provider: Native}, errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), Single(r), RunOptions{Prompt: "p", Provider: name}, Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	hops, _ := envelopeMap(t, res, err)["hops"].([]any)
	h, _ := hops[0].(map[string]any)
	if errs, _ := h["errors"].([]any); len(errs) != 1 {
		t.Errorf("hop = %v, want the recovered credential failure in errors", h)
	}
}

// Two batches started in the same millisecond still get distinct batch ids.
func TestNewBatchIDDistinctAtOneInstant(t *testing.T) {
	old := batchNow
	batchNow = func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { batchNow = old })
	a, b := NewBatchID(), NewBatchID()
	if a == b || !strings.HasPrefix(a, "batch-20261029T120000.000") {
		t.Errorf("ids %q and %q: want distinct ids that keep the batch-<stamp> prefix", a, b)
	}
}

type cancelAfterRun struct {
	name   string
	cancel context.CancelFunc
}

func (c cancelAfterRun) Name() string     { return c.name }
func (c cancelAfterRun) Caps() Caps       { return Caps{} }
func (c cancelAfterRun) Available() error { return nil }
func (c cancelAfterRun) Run(context.Context, RunOptions, Sink) (Result, error) {
	c.cancel() // the parent ctx dies at the moment the dispatch succeeds
	return Result{Answer: "a"}, nil
}

// A cancel that lands right after a dispatch succeeded must not overwrite its answer.
func TestSuccessSurvivesCancelRightAfterIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	c := Chain{Want: "first", Dispatches: []Dispatch{{Agent: cancelAfterRun{name: "first", cancel: cancel}}, {Agent: flagRunner{name: "second", runs: &runs}}}}
	res, err := RunWithFallback(ctx, c, RunOptions{Prompt: "p"}, Discard, nil)
	if err != nil || res.Answer != "a" || runs.Load() != 0 || len(res.Hops) != 1 {
		t.Fatalf("res=%+v err=%v second ran %d times; want the first dispatch's answer alone", res, err, runs.Load())
	}
}

// A copilot dispatch whose gate falls back to Databricks bills (and is capped on) that Databricks account, however the
// router keyed the dispatch.
func TestCopilotGateFallbackIsCappedOnTheDatabricksAccount(t *testing.T) {
	ghLogin(t, "alice", 99)
	t.Setenv("DBX_GATE_KEY", "dbx-secret")
	t.Cleanup(func() { vaulttest.Put(t, vault.Overlay, "providers", `{}`) }) // the default must not leak to later tests
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"alice","providers":{`+ghProfiles("alice")+`,"databricks":{"dbx":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","env":"DBX_GATE_KEY"},"models":{"opus":"m"}}}}}`)
	var cur, peak atomic.Int32
	var tasks []BatchTask
	for i := range DefaultPerKey + 2 {
		name := fmt.Sprintf("g%d", i)
		r := gaugeRunner{name: "copilot", cur: &cur, max: &peak}
		tasks = append(tasks, BatchTask{ID: name, Agent: "copilot", Req: RunOptions{Prompt: "p", Model: "alice:opus"},
			Prepare: func(context.Context) (*Prepared, error) {
				rec := Record{ID: "id-" + name, Agent: "copilot", Log: filepath.Join(t.TempDir(), "l.log")}
				return &Prepared{Opts: RunOptions{Prompt: "p", Model: "alice:opus"}, Record: rec,
					chain: Chain{Want: "copilot", Dispatches: []Dispatch{{Agent: r, Account: "github·" + name, keepCaller: true}}}}, nil
			}})
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: len(tasks), Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > DefaultPerKey || got < 2 {
		t.Errorf("peak concurrent copilot dispatches = %d, want 2..%d: they all bill the one Databricks account", got, DefaultPerKey)
	}
}

// A hub dispatch authenticates with the stored opencode login, so an absent registry credential must not fail its guard.
func TestHubDispatchGuardIgnoresAbsentRegistryCredential(t *testing.T) {
	vaulttest.UseKeyring(t)
	p := &providers.Provider{Name: "ocgo", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: "key", Command: "false"}}
	disp := hubDispatch{agent: "opencode", provider: p}
	if _, gerr := guardAttempt(withIsolation(context.Background()), disp, RunOptions{}); gerr != nil {
		t.Errorf("guardAttempt = %v, want the hub dispatch to pass: its credential is the opencode login", gerr)
	}
}

// gateFallbackEnv: the copilot GitHub login is past its gate and one Databricks profile (opus/sonnet ids) takes over.
func gateFallbackEnv(t *testing.T) {
	t.Helper()
	ghLogin(t, "alice", 99)
	t.Setenv("DBX_GATE_KEY", "dbx-secret")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{`+ghProfiles("alice")+`,"databricks":{"dbx":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","env":"DBX_GATE_KEY"},"models":{"opus":"dbx-opus","sonnet":"dbx-sonnet"}}}}}`)
}

// A frozen copilot target keeps its provider but follows the attempt's model: after a model-denied opus attempt the
// retry runs on sonnet.
func TestFrozenCopilotTargetFollowsTheDowngradedModel(t *testing.T) {
	fastDefaultBackoff(t)
	gateFallbackEnv(t)
	fakeCopilotBin(t)
	var envs [][]string
	prev := copilotAsk
	copilotAsk = func(_ context.Context, o copilotsdk.Options, _ string, _ []string) (copilotsdk.Result, error) {
		envs = append(envs, slices.Clone(o.Env))
		if len(envs) == 1 {
			return copilotsdk.Result{}, errors.New("HTTP 429: insufficient_quota, limit: 0")
		}
		return copilotsdk.Result{Text: "hi", SessionID: "s"}, nil
	}
	t.Cleanup(func() { copilotAsk = prev })
	res, err := RunWithFallback(context.Background(), Single(copilotAgent), RunOptions{Prompt: "p", Model: "opus", Direct: true}, Discard, nil)
	if err != nil || res.Answer != "hi" || len(envs) != 2 {
		t.Fatalf("res=%+v err=%v calls=%d; want the retry to answer", res, err, len(envs))
	}
	has := func(env []string, kv string) bool { return slices.Contains(env, kv) }
	if !has(envs[0], "COPILOT_MODEL=dbx-opus") || !has(envs[1], "COPILOT_MODEL=dbx-sonnet") {
		t.Errorf("models by attempt = %v / %v, want dbx-opus then the downgraded dbx-sonnet", envs[0], envs[1])
	}
}

// A quota failure on a gate-fallback dispatch is remembered with the Databricks account's credits window, not the
// subscription window of the GitHub login the router keyed it under.
func TestGateFallbackDispatchBillsCreditsWindow(t *testing.T) {
	gateFallbackEnv(t)
	var account string
	var until, at time.Time
	prev := markExhausted
	markExhausted = func(a string, u, now time.Time, _ string) error { account, until, at = a, u, now; return nil }
	t.Cleanup(func() { markExhausted = prev })
	quota := errors.New("402 Payment Required")
	r := &fakeRunner{name: "copilot", errs: []error{quota}, emptyAnswer: true}
	c := Chain{Want: "copilot", Dispatches: []Dispatch{{Agent: r, Account: "github·alice", Billing: router.BillSubscription, keepCaller: true}}}
	_, _ = RunWithFallback(context.Background(), c, RunOptions{Prompt: "p", Model: "alice:opus"}, Discard, nil)
	want := router.ResetFor(quota.Error(), router.BillCredits, router.Entry{}, at)
	if account != "databricks·dbx" || !until.Equal(want) {
		t.Errorf("marked %q until %v, want databricks·dbx until the credits reset %v", account, until, want)
	}
}

// A cooldown that idles the dispatch before attempt 1 is followed by a recheck: an account marked auth-dead meanwhile
// is not attempted.
func TestFirstAttemptRechecksAccountAfterCooldownIdle(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var once sync.Once
	prev := loadState
	loadState = func() router.State {
		once.Do(func() { coolDown("late·A", 200*time.Millisecond) })
		return prev()
	}
	t.Cleanup(func() { loadState = prev })
	var runs atomic.Int32
	tasks := []BatchTask{
		acctTask(t, "late", "late·A", flagRunner{name: "late", runs: &runs}),
		acctTask(t, "marker", "free·B", markingRunner{name: "marker", mark: func() { _ = router.MarkAuth("late·A", "login expired") }}),
	}
	var out strings.Builder
	RunBatch(context.Background(), tasks, BatchOptions{Jobs: 1, Out: &out})
	if runs.Load() != 0 {
		t.Errorf("the first attempt ran %d times on an account marked auth-dead during its cooldown wait", runs.Load())
	}
}

// Own-login dispatches (no account) on one agent still share DefaultPerKey, even when --jobs allows more.
func TestBatchCapsOwnLoginDispatchesPerAgent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var cur, peak atomic.Int32
	r := gaugeRunner{name: "opencode", cur: &cur, max: &peak}
	var tasks []BatchTask
	for i := range DefaultPerKey + 3 {
		id := fmt.Sprintf("own%d", i)
		tasks = append(tasks, BatchTask{ID: id, Agent: "opencode", Req: RunOptions{Prompt: "p"},
			Prepare: func(context.Context) (*Prepared, error) {
				return &Prepared{Opts: RunOptions{Prompt: "p"},
					chain:  Chain{Want: "opencode", Dispatches: []Dispatch{{Agent: r, keepCaller: true}}},
					Record: Record{ID: "id-" + id, Agent: "opencode", Log: filepath.Join(t.TempDir(), "l.log")}}, nil
			}})
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: len(tasks), Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > DefaultPerKey || got < 2 {
		t.Errorf("peak concurrent own-login dispatches = %d, want 2..%d", got, DefaultPerKey)
	}
}
