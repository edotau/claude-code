package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilotsdk"
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
func (g gaugeRunner) Run(context.Context, Request, io.Writer) (Result, error) {
	n := g.cur.Add(1)
	for m := g.max.Load(); n > m && !g.max.CompareAndSwap(m, n); m = g.max.Load() {
	}
	time.Sleep(60 * time.Millisecond)
	g.cur.Add(-1)
	return Result{Answer: "a"}, nil
}

// Tasks queued under different agent names but legs billed to one account never run more than DefaultPerKey at once.
func TestBatchCapsConcurrencyPerBilledAccount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var cur, peak atomic.Int32
	var tasks []BatchTask
	for i := range DefaultPerKey + 3 {
		name := fmt.Sprintf("agent%d", i)
		r := gaugeRunner{name: name, cur: &cur, max: &peak}
		tasks = append(tasks, BatchTask{ID: name, Agent: name, Req: Request{Prompt: "p"},
			Prepare: func(context.Context) (Chain, Record, error) {
				return Chain{Want: name, Legs: []Leg{{Agent: r, Account: "shared·acct", keepCaller: true}}}, Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}, nil
			}})
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: len(tasks), Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > DefaultPerKey || got < 2 {
		t.Errorf("peak concurrent legs on one account = %d, want 2..%d", got, DefaultPerKey)
	}
}

// A routed provider:model request on a vendor agent is guarded as that provider, not skipped as an own login.
func TestGuardProviderResolvesModelPrefixForVendorAgent(t *testing.T) {
	hmProfile(t, "true")
	p, err := guardProvider(&fakeRunner{name: "codex"}, Request{Model: "hm:opus"})
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
	r := &fakeRunner{name: "codex", caps: Caps{Model: true}, errs: []error{nil}}
	_, err := Run(withIsolation(context.Background()), r, Request{Prompt: "p", Model: "hm:opus"}, io.Discard, nil)
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
	r := &fakeRunner{name: "claude", caps: Caps{Provider: true}, errs: []error{nil}}
	res, err := Run(context.Background(), r, Request{Prompt: "p", Provider: name}, io.Discard, nil)
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
	copilotAsk = func(_ context.Context, o copilotsdk.Options, _ string) (copilotsdk.Result, error) {
		mu.Lock()
		seen = slices.Clone(o.Env)
		mu.Unlock()
		return copilotsdk.Result{Text: "hi", SessionID: "s"}, nil
	}
	t.Cleanup(func() { copilotAsk = prev })
	if _, err := Run(withIsolation(context.Background()), copilotRunner{}, Request{Prompt: "p", Provider: "alice", Direct: true}, io.Discard, nil); err != nil {
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
func (s slowRunner) Run(context.Context, Request, io.Writer) (Result, error) {
	if s.boom {
		panic("boom")
	}
	time.Sleep(s.sleep)
	return Result{Answer: "a"}, nil
}

// acctTask is a batch task whose single leg bills account.
func acctTask(t *testing.T, name, account string, r Runner) BatchTask {
	return BatchTask{ID: name, Agent: name, Req: Request{Prompt: "p"},
		Prepare: func(context.Context) (Chain, Record, error) {
			rec := Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}
			return Chain{Want: name, Legs: []Leg{{Agent: r, Account: account, keepCaller: true}}}, rec, nil
		}}
}

// A panicking runner (recovered by the batch) must give its leg permit back: later tasks on the account still run.
func TestBatchPanickingLegsReleaseTheirPermits(t *testing.T) {
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
		tasks = append(tasks, acctTask(t, name, "A·acct", slowRunner{name: name, sleep: 400 * time.Millisecond}))
	}
	tasks = append(tasks, acctTask(t, "b", "B·acct", slowRunner{name: "b"}))
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: n, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if first := strings.SplitN(out.String(), "\n", 2)[0]; !strings.Contains(first, `"task_id":"b"`) {
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
	first := &fakeRunner{name: "claude", caps: Caps{Provider: true}, errs: []error{nil}}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	c := Chain{Want: "claude", Legs: []Leg{{Agent: first, Account: "claude:" + name, keepCaller: true}, {Agent: second}}}
	res, err := RunWithFallback(context.Background(), c, Request{Prompt: "p"}, io.Discard, nil)
	if err != nil || res.Answer != "a" || len(first.seen) != 0 || len(second.seen) != 1 {
		t.Fatalf("res=%+v err=%v first=%d second=%d; want hop 2 to answer", res, err, len(first.seen), len(second.seen))
	}
	if len(res.Hops) != 2 || res.Hops[0].ErrorKind != "auth" {
		t.Errorf("hops = %+v, want hop 1 error_kind auth", res.Hops)
	}
}

// legGauge is a runner that fails (as auth) or answers after a pause, tracking one gauge shared by every leg.
type legGauge struct {
	name     string
	fail     bool
	cur, max *atomic.Int32
}

func (g legGauge) Name() string     { return g.name }
func (g legGauge) Caps() Caps       { return Caps{} }
func (g legGauge) Available() error { return nil }
func (g legGauge) Run(context.Context, Request, io.Writer) (Result, error) {
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

// Legs in flight never exceed --jobs while tasks fall back from a full account A to account B.
func TestBatchJobsCapHoldsAcrossFallbackHops(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const jobs = 5
	var cur, peak atomic.Int32
	var tasks []BatchTask
	for i := range 12 {
		name := fmt.Sprintf("f%d", i)
		first, second := legGauge{name: "codex", fail: true, cur: &cur, max: &peak}, legGauge{name: "gemini", cur: &cur, max: &peak}
		tasks = append(tasks, BatchTask{ID: name, Agent: name, Req: Request{Prompt: "p"}, // distinct keys: the old per-key gate would cap at 4
			Prepare: func(context.Context) (Chain, Record, error) {
				rec := Record{ID: "id-" + name, Agent: "codex", Log: filepath.Join(t.TempDir(), "l.log")}
				return Chain{Want: "codex", Legs: []Leg{{Agent: first, Account: "A·acct", keepCaller: true}, {Agent: second, Account: "B·acct"}}}, rec, nil
			}})
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: jobs, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > jobs || got < 2 {
		t.Errorf("peak in-flight legs = %d, want 2..%d", got, jobs)
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
func (f flagRunner) Run(context.Context, Request, io.Writer) (Result, error) {
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
	second.Prepare = func(ctx context.Context) (Chain, Record, error) { prepared.Add(1); return inner(ctx) }
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
	failFastPair(t, BatchTask{ID: "first", Agent: "first", Req: Request{Prompt: "p"},
		Prepare: func(context.Context) (Chain, Record, error) { return Chain{}, Record{}, errors.New("route failed") }})
}

// --fail-fast with a panicking leg: the next task never spawns.
func TestBatchFailFastPanickingLegStopsQueuedTask(t *testing.T) {
	failFastPair(t, acctTask(t, "first", "ff·acct", slowRunner{name: "first", boom: true}))
}

// A task waiting out its account's cooldown holds nothing: a task on another account runs and finishes first.
func TestBatchCoolingAccountDoesNotBlockOthers(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	coolDown("cool·A", 700*time.Millisecond)
	tasks := []BatchTask{
		acctTask(t, "cooling", "cool·A", slowRunner{name: "cooling"}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: 2, Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if first := strings.SplitN(out.String(), "\n", 2)[0]; !strings.Contains(first, `"task_id":"free"`) {
		t.Errorf("first envelope = %s, want the free-account task", first)
	}
}
