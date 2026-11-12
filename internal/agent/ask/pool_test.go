package ask

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/inflight"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// recvWithin returns the next value on ch or fails the test with msg after d.
func recvWithin[T any](t *testing.T, ch <-chan T, d time.Duration, msg string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatal(msg)
		panic("unreachable")
	}
}

// vendorHost isolates the config dir, seeds a provider and installs a fake opencode that fails on a FAILME prompt.
func vendorHost(t *testing.T) {
	t.Helper()
	skipWithoutSh(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	doc := `{"providers":{"openai":{"acme":{"protocol":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}}}}`
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatal(err)
	}
	pathstest.FakeVendor(t, "opencode", "p=$(cat); case \"$p $*\" in *FAILME*) echo boom >&2; exit 1;; esac\n"+
		`printf '%s\n' '{"type":"text","sessionID":"s","part":{"text":"hi"}}'`+"\n")
}

// vendorPrepare routes a real opencode dispatch for prompt and counts how often it is called.
func vendorPrepare(t *testing.T, prompt string, n *atomic.Int32) func(context.Context) (*agent.Prepared, error) {
	dir := t.TempDir()
	return func(ctx context.Context) (*agent.Prepared, error) {
		n.Add(1)
		return agent.NewClient(nil, nil, nil).Prepare(ctx, agent.RunOptions{Agent: "opencode", Provider: "acme",
			Model: "acme-large", Prompt: prompt, WorkDir: dir, Fallback: router.FallbackAgents}, nil)
	}
}

// vendorTask is a task whose first attempt runs the first prompt and whose retry, when given, runs the second.
func vendorTask(t *testing.T, id, plan, first, retry string, prep, again *atomic.Int32) batchTask {
	task := batchTask{ID: id, Agent: "opencode", Plan: plan, Prepare: vendorPrepare(t, first, prep)}
	if retry != "" {
		task.Retry = vendorPrepare(t, retry, again)
	}
	return task
}

func failPrepare(msg string) func(context.Context) (*agent.Prepared, error) {
	return func(context.Context) (*agent.Prepared, error) { return nil, errors.New(msg) }
}

func TestRunTasksPreparesOverlap(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	prepare := func(context.Context) (*agent.Prepared, error) {
		entered <- struct{}{}
		<-release
		return nil, errors.New("prepare failed")
	}
	var buf bytes.Buffer
	done := make(chan bool, 1)
	go func() {
		done <- runTasks(t.Context(), []batchTask{{ID: "a", Agent: "x", Prepare: prepare}, {ID: "b", Agent: "x", Prepare: prepare}},
			poolOptions{Jobs: 2, Out: &buf})
	}()
	for range 2 {
		recvWithin(t, entered, 5*time.Second, "both prepares must enter before either is released")
	}
	once.Do(func() { close(release) })
	if recvWithin(t, done, 5*time.Second, "runTasks did not finish after releasing prepares") {
		t.Error("runTasks returned true for prepare errors")
	}
	if envs := decodePoolEnvelopes(t, &buf); len(envs) != 2 {
		t.Fatalf("got %d envelopes, want 2", len(envs))
	}
}

// One error envelope per task a and b; under FailFast b is cancelled before its Prepare runs.
func TestRunTasksPrepareErrorEnvelope(t *testing.T) {
	for _, failFast := range []bool{false, true} {
		t.Run(fmt.Sprintf("failFast=%v", failFast), func(t *testing.T) {
			var secondCalls atomic.Int32
			tasks := []batchTask{
				{ID: "a", Agent: "x", Prepare: failPrepare("boom a")},
				{ID: "b", Agent: "x", Prepare: func(ctx context.Context) (*agent.Prepared, error) {
					secondCalls.Add(1)
					return failPrepare("boom b")(ctx)
				}},
			}
			var buf bytes.Buffer
			if runTasks(t.Context(), tasks, poolOptions{ID: "batch-t", Jobs: 1, FailFast: failFast, Out: &buf}) {
				t.Error("runTasks returned true for prepare errors")
			}
			want := map[string]string{"a": "boom a", "b": "boom b"}
			if failFast {
				want["b"] = "cancelled before start"
				if n := secondCalls.Load(); n != 0 {
					t.Errorf("second Prepare called %d times, want 0", n)
				}
			}
			checkPrepareEnvelopes(t, decodePoolEnvelopes(t, &buf), want)
		})
	}
}

// checkPrepareEnvelopes asserts one error envelope per task id in want, each carrying its message.
func checkPrepareEnvelopes(t *testing.T, envs []wire.Envelope, want map[string]string) {
	t.Helper()
	if len(envs) != len(want) {
		t.Fatalf("got %d envelopes, want %d", len(envs), len(want))
	}
	for _, env := range envs {
		msg, ok := want[env.TaskID]
		if !ok {
			t.Errorf("unexpected or duplicate task id %q", env.TaskID)
			continue
		}
		delete(want, env.TaskID)
		if env.BatchID != "batch-t" || env.Status != "error" || env.Agent != "x" || env.Error == nil {
			t.Fatalf("incorrect prepare failure envelope: %+v", env)
		}
		if !strings.Contains(env.Error.Message, msg) {
			t.Errorf("task %q error message missing %q: %+v", env.TaskID, msg, env.Error)
		}
	}
}

func decodePoolEnvelopes(t *testing.T, r io.Reader) []wire.Envelope {
	t.Helper()
	var envs []wire.Envelope
	dec := json.NewDecoder(r)
	for {
		var env wire.Envelope
		err := dec.Decode(&env)
		if errors.Is(err, io.EOF) {
			return envs
		}
		if err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		envs = append(envs, env)
	}
}

// A task that reaches run leaves a record carrying its task id, the shared batch id and its plan_task.
func TestRunTasksStampsRecordIDs(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	mk := func(id, plan string, off int) batchTask {
		return batchTask{ID: id, Agent: "codex", Plan: plan, Prepare: func(context.Context) (*agent.Prepared, error) {
			rec := agent.NewRecord("codex", agent.RunOptions{Prompt: id}, base.Add(time.Duration(off)*time.Second))
			return &agent.Prepared{Record: rec}, nil
		}}
	}
	var buf bytes.Buffer
	runTasks(t.Context(), []batchTask{mk("a", "Phase 1.1", 0), mk("b", "", 1)}, poolOptions{ID: "batch-x", Jobs: 1, Out: &buf})
	got := map[string]agent.Record{}
	for _, r := range agent.RecentRecords(10) {
		got[r.TaskID] = r
	}
	if len(got) != 2 {
		t.Fatalf("records by task id = %+v, want a and b", got)
	}
	if a := got["a"]; a.BatchID != "batch-x" || a.PlanTask != "Phase 1.1" {
		t.Errorf("record a = task %q batch %q plan %q", a.TaskID, a.BatchID, a.PlanTask)
	}
	if b := got["b"]; b.BatchID != "batch-x" || b.PlanTask != "" {
		t.Errorf("record b = batch %q plan %q, want batch-x and no plan", b.BatchID, b.PlanTask)
	}
}

// Tasks sent on More run while the batch is open; runTasks returns only after More closes and every task ends.
func TestRunTasksMoreClaimsTasks(t *testing.T) {
	vendorHost(t)
	var n atomic.Int32
	more := make(chan batchTask)
	var buf bytes.Buffer
	done := make(chan bool, 1)
	go func() {
		done <- runTasks(t.Context(), []batchTask{vendorTask(t, "a", "", "ok", "", &n, &n)}, poolOptions{ID: "batch-m", Jobs: 1, Out: &buf, More: more})
	}()
	more <- vendorTask(t, "b", "", "ok", "", &n, &n)
	more <- vendorTask(t, "c", "", "ok", "", &n, &n)
	select {
	case <-done:
		t.Fatal("runTasks returned before More was closed")
	case <-time.After(300 * time.Millisecond):
	}
	close(more)
	if !recvWithin(t, done, 10*time.Second, "runTasks did not return after More closed") {
		t.Error("runTasks = false, want every task ok")
	}
	var ids []string
	for _, env := range decodePoolEnvelopes(t, &buf) {
		if env.BatchID != "batch-m" || env.Status != wire.StatusOK {
			t.Errorf("envelope %+v: want ok and batch-m", env)
		}
		ids = append(ids, env.TaskID)
	}
	if got := strings.Join(ids, ","); got != "a,b,c" {
		t.Errorf("task ids in completion order = %q, want a,b,c (Jobs=1 runs serially)", got)
	}
}

// Under FailFast a failing initial task ends the batch: a task waiting on an open More never runs.
func TestRunTasksMoreFailFastDoesNotRunPending(t *testing.T) {
	vendorHost(t)
	var prepA, prepB, unused atomic.Int32
	more := make(chan batchTask, 1) // never closed: runTasks must end on the cancellation alone
	more <- vendorTask(t, "m", "", "ok", "", &prepB, &unused)
	var buf bytes.Buffer
	done := make(chan bool, 1)
	go func() {
		done <- runTasks(t.Context(), []batchTask{vendorTask(t, "a", "", "FAILME", "", &prepA, &unused)},
			poolOptions{ID: "batch-f", Jobs: 1, FailFast: true, Out: &buf, More: more})
	}()
	if recvWithin(t, done, 10*time.Second, "runTasks hung with More open under FailFast") {
		t.Error("runTasks = true, want false after the failure")
	}
	if prepB.Load() != 0 {
		t.Errorf("pending More task was prepared %d times, want 0", prepB.Load())
	}
	for _, env := range decodePoolEnvelopes(t, &buf) {
		if env.TaskID == "m" && (env.Error == nil || env.Error.Kind != "canceled") {
			t.Errorf("m envelope = %+v, want it never emitted or canceled", env)
		}
	}
}

// runOpts runs the JSONL lines through runBatch against the fake opencode vendor; returns envelopes by task id.
func runOpts(t *testing.T, initial string, o batchOpts) map[string][]wire.Envelope {
	t.Helper()
	lines, err := readTaskLines(strings.NewReader(initial))
	if err != nil {
		t.Fatal(err)
	}
	o.Jobs, o.Agent, o.Fallback = 1, "opencode", router.FallbackAgents
	base := agent.RunOptions{Provider: "acme", Model: "acme-large", WorkDir: t.TempDir()}
	var buf bytes.Buffer
	id, _ := runBatch(t.Context(), lines, o, base, false, func() *agent.Client { return agent.NewClient(nil, nil, nil) }, &buf)
	byID := map[string][]wire.Envelope{}
	for _, env := range decodePoolEnvelopes(t, &buf) {
		if env.BatchID != id {
			t.Errorf("envelope %s batch_id %q, want the run's %q", env.TaskID, env.BatchID, id)
		}
		byID[env.TaskID] = append(byID[env.TaskID], env)
	}
	return byID
}

// followLines feeds the given raw JSONL lines through build, as a follower would, then closes.
func followLines(raw string, firstN int) func(context.Context, func(parsedLine) batchTask) <-chan batchTask {
	return func(_ context.Context, build func(parsedLine) batchTask) <-chan batchTask {
		out := make(chan batchTask, 16)
		lines, _ := readTaskLines(strings.NewReader(raw))
		for i, l := range lines {
			l.N = firstN + i
			out <- build(l)
		}
		close(out)
		return out
	}
}

// A followed task is claimed under the run's batch id; a duplicate id and bad lines fail alone.
func TestRunFollowClaimsTasksAndBadLinesFailAlone(t *testing.T) {
	vendorHost(t)
	follow := followLines(strings.Join([]string{
		`{"id":"f","prompt":"ok"}`, `{"id":"a","prompt":"ok"}`, `{"id":"j"`, `{"id":"u","prompt":"ok","bogus":1}`, `{"id":"np"}`,
		`{"id":"g","prompt":"ok"}`}, "\n")+"\n", 2)
	got := runOpts(t, `{"id":"a","prompt":"ok"}`+"\n", batchOpts{Follow: follow})
	for _, id := range []string{"f", "g"} {
		if len(got[id]) != 1 || got[id][0].Status != wire.StatusOK {
			t.Errorf("task %s = %+v, want one ok envelope", id, got[id])
		}
	}
	for _, id := range []string{"u", "np", "t4"} { // the malformed line has no id: it is named after its line number
		if len(got[id]) != 1 || got[id][0].Status != wire.StatusError {
			t.Errorf("bad line %s = %+v, want one error envelope", id, got[id])
		}
	}
	var ok, failed int // the initial a runs; the followed duplicate a is a failure envelope under the same id
	for _, env := range got["a"] {
		if env.Status == wire.StatusOK {
			ok++
		} else {
			failed++
		}
	}
	if ok != 1 || failed != 1 {
		t.Errorf("task a: %d ok and %d failed envelopes, want the initial run ok and the duplicate failed", ok, failed)
	}
}

// Decorate runs on every built task; a Retry it sets reroutes a failure once under the same id.
func TestRunDecorateSetsRetry(t *testing.T) {
	vendorHost(t)
	var again atomic.Int32
	decorate := func(task *batchTask) { task.Retry = vendorPrepare(t, "ok", &again) }
	got := runOpts(t, `{"id":"a","prompt":"FAILME"}`+"\n", batchOpts{Decorate: decorate})
	if len(got["a"]) != 1 || got["a"][0].Status != wire.StatusOK || len(got["a"][0].Hops) < 2 || again.Load() != 1 {
		t.Errorf("a = %+v retries %d, want one ok envelope with >= 2 hops after one retry", got["a"], again.Load())
	}
}

// With neither hook set runBatch is the plain pool: one task, one envelope, no retry.
func TestRunWithoutHooks(t *testing.T) {
	vendorHost(t)
	got := runOpts(t, `{"id":"a","prompt":"FAILME"}`+"\n"+`{"id":"b","prompt":"ok"}`+"\n", batchOpts{})
	if len(got["a"]) != 1 || got["a"][0].Status != wire.StatusError || len(got["a"][0].Hops) != 1 ||
		len(got["b"]) != 1 || got["b"][0].Status != wire.StatusOK {
		t.Errorf("envelopes = %+v, want a failed once without retry and b ok", got)
	}
}

// runRetryBatch runs tasks with one job and returns the ok flag and the decoded envelopes by task id.
func runRetryBatch(t *testing.T, o poolOptions, tasks ...batchTask) (bool, map[string][]wire.Envelope) {
	t.Helper()
	var buf bytes.Buffer
	o.Out, o.ID, o.Jobs = &buf, "batch-r", 1
	ok := runTasks(t.Context(), tasks, o)
	byID := map[string][]wire.Envelope{}
	for _, env := range decodePoolEnvelopes(t, &buf) {
		byID[env.TaskID] = append(byID[env.TaskID], env)
	}
	return ok, byID
}

// A real failure is retried once under the same task id: one envelope, the retry's, both attempts in hops.
func TestRetryAfterFailureEmitsOneEnvelope(t *testing.T) {
	vendorHost(t)
	var prep, again atomic.Int32
	_, got := runRetryBatch(t, poolOptions{}, vendorTask(t, "a", "Phase 1.1", "FAILME", "ok", &prep, &again))
	if len(got["a"]) != 1 || got["a"][0].Status != wire.StatusOK || len(got["a"][0].Hops) < 2 {
		t.Fatalf("envelopes for a = %+v, want one ok envelope with >= 2 hops", got["a"])
	}
	if prep.Load() != 1 || again.Load() != 1 {
		t.Errorf("prepare/retry calls = %d/%d, want 1/1", prep.Load(), again.Load())
	}
	recs := agent.RecentRecords(10)
	for _, r := range recs {
		if r.TaskID != "a" || r.BatchID != "batch-r" || r.PlanTask != "Phase 1.1" {
			t.Errorf("record %s = task %q batch %q plan %q", r.ID, r.TaskID, r.BatchID, r.PlanTask)
		}
	}
	if len(recs) != 2 {
		t.Errorf("%d dispatch records, want 2 (first attempt and retry)", len(recs))
	}
}

// A retry that fails too is the task's one envelope, and the task never runs a third time.
func TestRetryAlsoFailsEmitsRetryError(t *testing.T) {
	vendorHost(t)
	var prep, again atomic.Int32
	ok, got := runRetryBatch(t, poolOptions{}, vendorTask(t, "a", "", "FAILME", "FAILME", &prep, &again))
	if ok || len(got["a"]) != 1 || got["a"][0].Status != wire.StatusError || len(got["a"][0].Hops) != 2 {
		t.Fatalf("ok=%v envelopes for a = %+v, want one error envelope with 2 hops", ok, got["a"])
	}
	if prep.Load() != 1 || again.Load() != 1 {
		t.Errorf("prepare/retry calls = %d/%d, want 1/1", prep.Load(), again.Load())
	}
}

// With one slot a retried task keeps its slot: later tasks still run and the batch ends.
func TestRetryJobsOneKeepsSlotAccounting(t *testing.T) {
	vendorHost(t)
	var n, again atomic.Int32
	done := make(chan map[string][]wire.Envelope, 1)
	go func() {
		_, got := runRetryBatch(t, poolOptions{},
			vendorTask(t, "a", "", "FAILME", "ok", &n, &again),
			vendorTask(t, "b", "", "ok", "", &n, &n),
			vendorTask(t, "c", "", "ok", "", &n, &n))
		done <- got
	}()
	got := recvWithin(t, done, 20*time.Second, "batch deadlocked: a retried task leaked its slot")
	for _, id := range []string{"a", "b", "c"} {
		if len(got[id]) != 1 || got[id][0].Status != wire.StatusOK {
			t.Errorf("task %s envelopes = %+v, want one ok", id, got[id])
		}
	}
}

// No retry without a dispatch (no hops), after a prepare error, after cancellation, or under FailFast.
func TestRetrySkippedWhenNotRealFailure(t *testing.T) {
	vendorHost(t)
	var retried atomic.Int32
	retry := func(context.Context) (*agent.Prepared, error) { retried.Add(1); return nil, errors.New("retry ran") }
	noHops := func(context.Context) (*agent.Prepared, error) {
		return &agent.Prepared{Record: agent.NewRecord("opencode", agent.RunOptions{}, time.Now())}, nil
	}
	runRetryBatch(t, poolOptions{},
		batchTask{ID: "h", Agent: "opencode", Prepare: noHops, Retry: retry},
		batchTask{ID: "p", Agent: "opencode", Prepare: failPrepare("prepare failed"), Retry: retry})
	if retried.Load() != 0 {
		t.Errorf("Retry ran %d times for a no-hop failure and a prepare error, want 0", retried.Load())
	}
	var prep, again atomic.Int32
	_, got := runRetryBatch(t, poolOptions{FailFast: true},
		vendorTask(t, "a", "", "FAILME", "ok", &prep, &again), vendorTask(t, "b", "", "ok", "", &prep, &prep))
	if again.Load() != 0 || len(got["a"]) != 1 || got["a"][0].Status != wire.StatusError {
		t.Errorf("FailFast: retry calls %d, a envelopes %+v, want 0 retries and a's own error", again.Load(), got["a"])
	}
}

// A batch cancelled while the first attempt runs does not start the retry.
func TestRetrySkippedWhenCancelled(t *testing.T) {
	vendorHost(t)
	var prep, again atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	task := vendorTask(t, "a", "", "FAILME", "ok", &prep, &again)
	inner := task.Prepare
	task.Prepare = func(c context.Context) (*agent.Prepared, error) {
		p, err := inner(c)
		cancel()
		return p, err
	}
	var buf bytes.Buffer
	runTasks(ctx, []batchTask{task}, poolOptions{ID: "batch-c", Jobs: 1, Out: &buf})
	if again.Load() != 0 || len(decodePoolEnvelopes(t, &buf)) != 1 {
		t.Errorf("retry calls = %d, want 0 and one envelope", again.Load())
	}
}

// A batch task's own profile reaches the request and prepares; an unknown one fails before any run.
func TestBatchTaskProfileReachesRequest(t *testing.T) {
	profileFixture(t)
	build := func(n int, profile string) batchTask {
		line := parsedLine{N: n, Task: batchLine{Prompt: "p", Profile: profile}}
		bt, err := buildTask(line, batchOpts{Agent: "codex", Fallback: router.FallbackAll}, agent.RunOptions{}, false, t.TempDir(), map[string]bool{}, newClient)
		if err != nil {
			t.Fatal(err)
		}
		return bt
	}
	bt := build(1, "acme")
	if bt.Req.Provider != "acme" {
		t.Fatalf("task request provider = %q, want acme", bt.Req.Provider)
	}
	if _, err := bt.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := build(2, "no-such-profile").Prepare(context.Background()); err == nil {
		t.Error("unknown batch profile prepared a chain, want an error before any run")
	}
}

// batchFile writes one JSONL task per line and returns the path.
func batchFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// envelopes decodes one envelope per non-empty stdout line.
func envelopes(t *testing.T, out string) []map[string]any {
	t.Helper()
	var all []map[string]any
	for ln := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if ln == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("stdout line is not an envelope: %v\n%s", err, ln)
		}
		all = append(all, e)
	}
	return all
}

func taskLine(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// cliTask is a JSONL claude task with its own workdir and tools; kv pairs override fields, a nil value drops one.
func cliTask(t *testing.T, id, prompt string, kv ...any) string {
	t.Helper()
	m := map[string]any{"id": id, "agent": "claude", "prompt": prompt, "workdir": t.TempDir(), "tools": true}
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(m, kv[i].(string))
		} else {
			m[kv[i].(string)] = kv[i+1]
		}
	}
	return taskLine(t, m)
}

func errKind(e map[string]any) string {
	if x, ok := e["error"].(map[string]any); ok {
		s, _ := x["kind"].(string)
		return s
	}
	return ""
}

// Spanning: mixed ok/error tasks stream one envelope each in completion order, tagged, with a non-zero exit.
func TestBatchMixedStreamsEnvelopesAndExitsNonZero(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	pathstest.FakeVendor(t, "claude", `p=$(cat); case "$p" in *SLOW*) sleep 0.3;; esac`+"\n"+
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"sc","model":"fake-claude"}'`+"\n"+
		`printf '%s\n' '{"type":"result","result":"hello","usage":{"input_tokens":3,"output_tokens":2}}'`)
	file := batchFile(t, cliTask(t, "slow", "SLOW task"), cliTask(t, "fast", "quick task"),
		cliTask(t, "bad", "x", "agent", "no-such-agent", "tools", nil), `{not json`)
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "2")
	if code == 0 {
		t.Fatalf("exit 0 with failing tasks\nstdout: %s\nstderr: %s", out, stderr)
	}
	envs := envelopes(t, out)
	if len(envs) != 4 {
		t.Fatalf("got %d envelopes, want 4 (one per input line, bad lines included)\n%s", len(envs), out)
	}
	checkMixedBatchResults(t, indexBatchEnvelopes(t, envs), envs, out)
}

func checkMixedBatchResults(t *testing.T, byID map[string]map[string]any, envs []map[string]any, out string) {
	t.Helper()
	for _, id := range []string{"slow", "fast", "bad"} {
		if byID[id] == nil {
			t.Fatalf("no envelope tagged task_id %q\n%s", id, out)
		}
	}
	for _, id := range []string{"slow", "fast"} {
		if byID[id]["status"] != "ok" || byID[id]["answer"] != "hello" {
			t.Errorf("task %s = %v, want status ok / answer hello", id, byID[id])
		}
	}
	if byID["bad"]["status"] == "ok" || errKind(byID["bad"]) != "usage" {
		t.Errorf("bad agent = %v, want a non-ok usage-kind envelope", byID["bad"])
	}
	pos := map[string]int{} // completion order: the SLOW task finishes after the fast one
	for i, e := range envs {
		id, _ := e["task_id"].(string)
		pos[id] = i
	}
	if pos["fast"] > pos["slow"] {
		t.Errorf("fast task printed after the slow one: streaming must be completion order\n%s", out)
	}
}

func indexBatchEnvelopes(t *testing.T, envs []map[string]any) map[string]map[string]any {
	t.Helper()
	byID := map[string]map[string]any{}
	batchIDs := map[string]bool{}
	dispatchIDs := map[string]bool{}
	for _, e := range envs {
		if e["schema"] != float64(1) {
			t.Errorf("schema = %v, want 1", e["schema"])
		}
		if b, _ := e["batch_id"].(string); b != "" {
			batchIDs[b] = true
		} else {
			t.Errorf("envelope without batch_id: %v", e)
		}
		if id, _ := e["task_id"].(string); id != "" {
			byID[id] = e
		}
		if d, _ := e["dispatch_id"].(string); d != "" {
			if dispatchIDs[d] {
				t.Errorf("dispatch_id %q repeated across tasks", d)
			}
			dispatchIDs[d] = true
		}
	}
	if len(batchIDs) != 1 {
		t.Errorf("batch_ids = %v, want exactly one shared id", batchIDs)
	}
	return byID
}

// --fail-fast: the first fatal failure cancels the batch; a queued task reports cancelled and never spawns.
func TestBatchFailFastNeverSpawnsQueuedTask(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	pathstest.FakeVendor(t, "claude", "cat > /dev/null; echo boom >&2; exit 1")
	marker := cliMarker(t, "opencode", `printf '%s\n' '{"type":"text","sessionID":"so","part":{"text":"hello"}}'`)
	file := batchFile(t, cliTask(t, "first", "fail"), cliTask(t, "queued", "never", "agent", "opencode"))
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "1", "--fail-fast")
	if code == 0 {
		t.Fatalf("exit 0 after a fatal failure\nstdout: %s\nstderr: %s", out, stderr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("queued task spawned its child after --fail-fast cancelled the batch")
	}
	var queued map[string]any
	for _, e := range envelopes(t, out) {
		if e["task_id"] == "queued" {
			queued = e
		}
	}
	if queued == nil {
		t.Fatalf("queued task got no envelope\n%s", out)
	}
	msg, _ := queued["error"].(map[string]any)["message"].(string)
	if queued["status"] != "error" || !strings.Contains(strings.ToLower(msg), "cancel") {
		t.Errorf("queued = %v, want status error with a cancelled message", queued)
	}
}

// profileVault isolates the CLI env and seeds anthropic profiles pa and pb on distinct local routes.
func profileVault(t *testing.T) {
	t.Helper()
	skipWithoutSh(t)
	cliEnv(t)
	vaulttest.Use(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	row := func(name, port, env, model string) string {
		return `"` + name + `":{"protocol":"anthropic","base_url":"http://127.0.0.1:` + port + `","auth":{"type":"key","env":"` + env +
			`"},"models":{"opus":"` + model + `","sonnet":"` + model + `","haiku":"` + model + `"}}`
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		row("pa", "11111", "PA_BATCH_KEY", "m-a")+","+row("pb", "22222", "PB_BATCH_KEY", "m-b")+`}}}`)
}

// Guarantee 1: concurrent tasks on two profiles; each child's env holds only its own route and token.
func TestBatchConcurrentProfilesKeepEnvsApart(t *testing.T) {
	profileVault(t)
	t.Setenv("PA_BATCH_KEY", "TOKEN-AAAA-1111")
	t.Setenv("PB_BATCH_KEY", "TOKEN-BBBB-2222")
	dump := t.TempDir()
	// each child announces itself, then waits until both are running: proof the two really overlap
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\n"+
		"env > "+dump+"/$$.env\ntouch "+dump+"/$$.up\n"+
		"for i in $(seq 1 100); do [ $(ls "+dump+"/*.up 2>/dev/null | wc -l) -ge 2 ] && break; sleep 0.05; done\n"+
		`printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t, cliTask(t, "a", "x", "profile", "pa"), cliTask(t, "b", "y", "profile", "pb"))
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "2")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	dumps, _ := filepath.Glob(filepath.Join(dump, "*.env"))
	if len(dumps) != 2 {
		t.Fatalf("got %d child env dumps, want 2 (the children never overlapped?)", len(dumps))
	}
	seen := map[string]bool{}
	for _, f := range dumps {
		raw, _ := os.ReadFile(f)
		recordBatchChildProfile(t, seen, string(raw))
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("children seen = %v, want one per profile", seen)
	}
}

func recordBatchChildProfile(t *testing.T, seen map[string]bool, env string) {
	t.Helper()
	a, b := strings.Contains(env, "TOKEN-AAAA-1111"), strings.Contains(env, "TOKEN-BBBB-2222")
	ra, rb := strings.Contains(env, "127.0.0.1:11111"), strings.Contains(env, "127.0.0.1:22222")
	switch {
	case a && !b && ra && !rb:
		seen["a"] = true
	case b && !a && rb && !ra:
		seen["b"] = true
	default:
		t.Errorf("child env mixes profiles (token a=%v b=%v, route a=%v b=%v)", a, b, ra, rb)
	}
}

// Guarantee 2: a credential issued for another host than the route's fails as auth before the child starts.
func TestBatchHostMismatchFailsAuthBeforeExec(t *testing.T) {
	cliEnv(t)
	vaulttest.Use(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("HM_BATCH_KEY", hosttest.UnsignedJWT(`{"iss":"https://adb-2.azuredatabricks.net"}`))
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"hm":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net",`+
		`"auth":{"type":"key","env":"HM_BATCH_KEY"},"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5","haiku":"databricks-claude-haiku-4-5"}}}}}`)
	marker := cliMarker(t, "claude", `printf '%s\n' '{"type":"result","result":"hello"}'`)
	code, out, stderr := runCLI(t, "ask", "--batch", batchFile(t, cliTask(t, "m", "x", "profile", "hm")))
	if code == 0 {
		t.Fatalf("exit 0 on a host mismatch\nstdout: %s\nstderr: %s", out, stderr)
	}
	envs := envelopes(t, out)
	if len(envs) != 1 || envs[0]["status"] == "ok" || errKind(envs[0]) != "auth" {
		t.Fatalf("envelopes = %v, want one non-ok envelope of kind auth", envs)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the child started although the credential's host does not match the route")
	}
	if strings.Contains(out+stderr, "adb-2") && strings.Contains(out+stderr, ".sig") {
		t.Error("output carries the raw credential")
	}
}

// An all-ok batch exits 0, and every task goes through runRecorded: an <id>.events.jsonl running start to result.
func TestBatchTasksWriteEventFiles(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t, cliTask(t, "one", "p1"), cliTask(t, "two", "p2"))
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "2")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	for _, e := range envelopes(t, out) {
		if e["status"] != "ok" {
			t.Errorf("envelope %v, want ok", e)
		}
	}
	files, _ := filepath.Glob(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "state", "harness", "dispatch", "*.events.jsonl"))
	if len(files) != 2 {
		t.Fatalf("event files = %v, want 2 (one per task)", files)
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) < 2 || !strings.Contains(lines[0], `"type":"start"`) || !strings.Contains(lines[len(lines)-1], `"type":"result"`) {
			t.Errorf("%s: want start...result, got\n%s", f, b)
		}
	}
}

// The vault's custom env holds every profile's vars; a batch child on pa gets pa's and never pb's credential var.
func TestBatchChildDropsOtherProfilesCustomCredentialVar(t *testing.T) {
	profileVault(t)
	t.Setenv("PA_BATCH_KEY", "TOKEN-AAAA-1111")
	for k, v := range map[string]string{"PB_BATCH_KEY": "PB-CUSTOM-SECRET", "PA_CUSTOM_NOTE": "pa-note"} {
		if err := providers.SetCustomEnv(k, v); err != nil {
			t.Fatal(err)
		}
	}
	dump := filepath.Join(t.TempDir(), "child.env")
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\nenv > "+dump+"\n"+`printf '%s\n' '{"type":"result","result":"hello"}'`)
	if code, out, stderr := runCLI(t, "ask", "--batch", batchFile(t, cliTask(t, "a", "x", "profile", "pa"))); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	env := string(raw)
	if strings.Contains(env, "PB-CUSTOM-SECRET") {
		t.Error("pa's child holds pb's custom credential var")
	}
	if !strings.Contains(env, "TOKEN-AAAA-1111") || !strings.Contains(env, "pa-note") {
		t.Errorf("pa's child lost its own token or an unrelated custom var:\n%s", env)
	}
}

// A batch whose stdout reader is gone stops spending: it cancels, exits non-zero, and its children die.
func TestBatchClosedStdoutCancelsAndReapsChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs unix signals and pipes")
	}
	cliEnv(t)
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	pathstest.FakeVendor(t, "claude", `p=$(cat); case "$p" in *SLOW*) echo $$ > `+pidfile+`; sleep 60;; esac`+"\n"+
		"for i in $(seq 1 100); do [ -s "+pidfile+" ] && break; sleep 0.05; done\n"+
		`printf '%s\n' '{"type":"result","result":"hello"}'`)
	args := []string{"--batch", batchFile(t, cliTask(t, "slow", "SLOW"), cliTask(t, "fast", "quick")), "--jobs", "2"}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close() // nobody reads the batch's stdout any more
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig; w.Close() })
	done := make(chan int, 1)
	go func() { done <- Cmd(args) }()
	select {
	case code := <-done:
		if code == 0 {
			t.Fatal("exit code 0, want non-zero: the batch lost its stdout")
		}
	case <-time.After(40 * time.Second):
		t.Fatal("the batch did not exit after its stdout closed")
	}
	checkBatchChildReaped(t, pidfile)
}

func checkBatchChildReaped(t *testing.T, pidfile string) {
	t.Helper()
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("the slow child never started: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	for range 60 {
		p, _ := os.FindProcess(pid)
		if err := p.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("child %d is still running after the batch exited", pid)
}

// runFn is the body of a poolRunner.
type runFn func(context.Context, wire.Sink) (wire.Result, error)

// poolRunner is an agent.Engine whose Run is its run func.
type poolRunner struct {
	name string
	run  runFn
}

func (p poolRunner) Name() string     { return p.name }
func (p poolRunner) Caps() agent.Caps { return agent.Caps{} }
func (p poolRunner) Available() error { return nil }
func (p poolRunner) Run(ctx context.Context, _ agent.RunOptions, w wire.Sink) (wire.Result, error) {
	return p.run(ctx, w)
}

var okResult = wire.Result{Answer: "a"}

func runner(name string, run runFn) agent.Engine { return poolRunner{name: name, run: run} }

func okRunner(name string) agent.Engine {
	return runner(name, func(context.Context, wire.Sink) (wire.Result, error) { return okResult, nil })
}

func boomRunner(name string) agent.Engine {
	return runner(name, func(context.Context, wire.Sink) (wire.Result, error) { panic("boom") })
}

// countRunner counts its runs.
func countRunner(name string, runs *atomic.Int32) agent.Engine {
	return runner(name, func(context.Context, wire.Sink) (wire.Result, error) { runs.Add(1); return okResult, nil })
}

// gaugeRunner records the peak concurrency on cur/peak, pauses, then answers or fails with err.
func gaugeRunner(name string, cur, peak *atomic.Int32, pause time.Duration, err error) agent.Engine {
	return runner(name, func(context.Context, wire.Sink) (wire.Result, error) {
		n := cur.Add(1)
		for m := peak.Load(); n > m && !peak.CompareAndSwap(m, n); m = peak.Load() {
		}
		time.Sleep(pause)
		cur.Add(-1)
		if err != nil {
			return wire.Result{}, err
		}
		return okResult, nil
	})
}

// markRunner runs mark, pauses and answers.
func markRunner(name string, mark func()) agent.Engine {
	return runner(name, func(context.Context, wire.Sink) (wire.Result, error) {
		mark()
		time.Sleep(50 * time.Millisecond)
		return okResult, nil
	})
}

// rateOnceRunner fails its first run with a 300ms rate limit, then answers.
func rateOnceRunner(name string, n *atomic.Int32) agent.Engine {
	return runner(name, func(context.Context, wire.Sink) (wire.Result, error) {
		if n.Add(1) == 1 {
			return wire.Result{}, &retry.Error{Kind: retry.KindRateLimit, Agent: name, RetryAfter: 300 * time.Millisecond, Err: errors.New("HTTP 429")}
		}
		return okResult, nil
	})
}

func markAuth(account string) func() {
	return func() { _ = router.MarkAuth(account, "login expired") }
}

// keepCaller is d's agent and account on agent.Single's dispatch, which keeps the caller's own --model.
func keepCaller(d agent.Dispatch) agent.Dispatch {
	k := agent.Single(d.Agent).Dispatches[0]
	k.Account = d.Account
	return k
}

// chainTask is a batch task for taskAgent whose prepared chain wants want and tries ds in order.
func chainTask(t *testing.T, id, taskAgent, want string, ds ...agent.Dispatch) batchTask {
	return batchTask{ID: id, Agent: taskAgent, Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*agent.Prepared, error) {
			rec := agent.Record{ID: "id-" + id, Agent: want, Log: filepath.Join(t.TempDir(), "l.log")}
			return &agent.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: rec, Chain: agent.Chain{Want: want, Dispatches: ds}}, nil
		}}
}

// acctTask is a batch task whose single dispatch bills account.
func acctTask(t *testing.T, name, account string, r agent.Engine) batchTask {
	return chainTask(t, name, name, name, keepCaller(agent.Dispatch{Agent: r, Account: account}))
}

// runPool runs tasks in a fresh config dir under a 10s deadline and returns the output and the batch result.
func runPool(t *testing.T, o poolOptions, tasks ...batchTask) (string, bool) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out strings.Builder
	o.Out = &out
	ok := runTasks(ctx, tasks, o)
	return out.String(), ok
}

// envLine is the output line of task id, or "".
func envLine(out, id string) string {
	for ln := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.Contains(ln, `"task_id":"`+id+`"`) {
			return ln
		}
	}
	return ""
}

func firstLine(out string) string { first, _, _ := strings.Cut(out, "\n"); return first }

// coolOnFirstLoad starts a cooldown on account the first time the agent loads router state.
func coolOnFirstLoad(t *testing.T, account string, d time.Duration) {
	var once sync.Once
	t.Cleanup(agent.SwapLoadState(func(prev func() router.State) func() router.State {
		return func() router.State {
			once.Do(func() { retry.CoolDown(account, d) })
			return prev()
		}
	}))
}

// Dispatches billed to one account, or own-login on one agent, never run more than agent.DefaultPerKey at once.
func TestBatchCapsConcurrency(t *testing.T) {
	cases := []struct {
		name, prefix, account string
		sameAgent             bool
	}{{"billed account across agent names", "agent", "shared·acct", false}, {"own login on one agent", "own", "", true}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cur, peak atomic.Int32
			var tasks []batchTask
			for i := range retry.DefaultPerKey + 3 {
				id := fmt.Sprintf("%s%d", c.prefix, i)
				ag := id
				if c.sameAgent {
					ag = "opencode"
				}
				r := gaugeRunner(ag, &cur, &peak, 60*time.Millisecond, nil)
				tasks = append(tasks, chainTask(t, id, ag, ag, keepCaller(agent.Dispatch{Agent: r, Account: c.account})))
			}
			if out, ok := runPool(t, poolOptions{Jobs: len(tasks)}, tasks...); !ok {
				t.Fatalf("batch failed: %s", out)
			}
			if got := int(peak.Load()); got > retry.DefaultPerKey || got < 2 {
				t.Errorf("peak concurrent dispatches = %d, want 2..%d", got, retry.DefaultPerKey)
			}
		})
	}
}

// A panicking agent (recovered by the batch) must give its dispatch permit back: later tasks on the account still run.
func TestBatchPanickingDispatchesReleaseTheirPermits(t *testing.T) {
	var tasks []batchTask
	for i := range retry.DefaultPerKey + 2 {
		name := fmt.Sprintf("p%d", i)
		tasks = append(tasks, acctTask(t, name, "panic·acct", boomRunner(name)))
	}
	out, ok := runPool(t, poolOptions{Jobs: 2}, tasks...)
	if ok {
		t.Fatal("batch reported ok although every agent panicked")
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(tasks) || strings.Contains(out, "cancel") {
		t.Fatalf("got %d envelopes (want %d, none cancelled): a lost permit blocked the account\n%s", len(lines), len(tasks), out)
	}
	if n := strings.Count(out, "panic: boom"); n != len(tasks) {
		t.Errorf("%d envelopes name the panic, want %d", n, len(tasks))
	}
}

// Tasks parked on a full account hold no pool slot: account-A tasks block until task b (idle account B) runs.
func TestBatchParkedTasksDoNotHoldPoolSlots(t *testing.T) {
	bRan := make(chan struct{})
	waiter := func(name string) agent.Engine {
		return runner(name, func(ctx context.Context, _ wire.Sink) (wire.Result, error) {
			select {
			case <-bRan:
			case <-ctx.Done():
				return wire.Result{}, ctx.Err()
			}
			return okResult, nil
		})
	}
	n := retry.DefaultPerKey + 2
	var tasks []batchTask
	for i := range n {
		name := fmt.Sprintf("a%d", i)
		tasks = append(tasks, acctTask(t, name, "A·acct", waiter(name)))
	}
	enterB := runner("b", func(context.Context, wire.Sink) (wire.Result, error) { close(bRan); return okResult, nil })
	tasks = append(tasks, acctTask(t, "b", "B·acct", enterB))
	if out, ok := runPool(t, poolOptions{Jobs: n}, tasks...); !ok {
		t.Fatalf("batch failed: task b never got a pool slot while account-A tasks were parked\n%s", out)
	}
}

// Dispatches in flight never exceed --jobs while tasks fall back from a full account A to account B.
func TestBatchJobsCapHoldsAcrossFallbackHops(t *testing.T) {
	const jobs = 5
	var cur, peak atomic.Int32
	var tasks []batchTask
	for i := range 12 { // distinct task agents: the old per-key gate would cap at 4
		name := fmt.Sprintf("f%d", i)
		first := gaugeRunner("codex", &cur, &peak, 40*time.Millisecond, errors.New("HTTP 401 unauthorized"))
		second := gaugeRunner("gemini", &cur, &peak, 40*time.Millisecond, nil)
		tasks = append(tasks, chainTask(t, name, name, "codex",
			keepCaller(agent.Dispatch{Agent: first, Account: "A·acct"}), agent.Dispatch{Agent: second, Account: "B·acct"}))
	}
	out, ok := runPool(t, poolOptions{Jobs: jobs}, tasks...)
	if !ok {
		t.Fatalf("batch failed: %s", out)
	}
	if got := int(peak.Load()); got > jobs || got < 2 {
		t.Errorf("peak in-flight dispatches = %d, want 2..%d", got, jobs)
	}
	if n := strings.Count(out, `"hops"`); n != len(tasks) {
		t.Errorf("%d envelopes carry hops, want %d (every task fell back)", n, len(tasks))
	}
}

// --fail-fast: whatever way the first task fails, the second neither prepares nor runs and reports cancelled.
func TestBatchFailFastStopsQueuedTask(t *testing.T) {
	cases := []struct {
		name  string
		first func(t *testing.T) batchTask
	}{
		{"panicking dispatch", func(t *testing.T) batchTask { return acctTask(t, "first", "ff·acct", boomRunner("first")) }},
		{"empty chain", func(t *testing.T) batchTask { return chainTask(t, "first", "first", "first") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var prepared, runs atomic.Int32
			second := acctTask(t, "second", "ff·acct", countRunner("second", &runs))
			inner := second.Prepare
			second.Prepare = func(ctx context.Context) (*agent.Prepared, error) { prepared.Add(1); return inner(ctx) }
			out, ok := runPool(t, poolOptions{Jobs: 1, FailFast: true}, c.first(t), second)
			if ok {
				t.Fatal("batch reported ok after a failed task")
			}
			if prepared.Load() != 0 || runs.Load() != 0 {
				t.Errorf("second task prepared %d times and ran %d times after --fail-fast tripped", prepared.Load(), runs.Load())
			}
			if !strings.Contains(out, `"task_id":"second"`) || !strings.Contains(out, "canceled") {
				t.Errorf("second task has no cancelled envelope:\n%s", out)
			}
		})
	}
}

// A task waiting out a cooldown or rate-limit backoff holds no permit: a task on another account finishes first.
func TestBatchWaitingAccountDoesNotBlockOthers(t *testing.T) {
	var rateRuns atomic.Int32
	cases := []struct {
		name  string
		jobs  int
		setup func(t *testing.T)
		id    string
		slow  agent.Engine
		acct  string
	}{
		{"cooling account", 2, func(*testing.T) { retry.CoolDown("cool·A", 250*time.Millisecond) }, "cooling", okRunner("cooling"), "cool·A"},
		{"backoff after 429", 1, func(*testing.T) {}, "limited", rateOnceRunner("limited", &rateRuns), "rate·A"},
		{"cooldown before first attempt", 1, func(t *testing.T) { coolOnFirstLoad(t, "late·A", 200*time.Millisecond) }, "late", okRunner("late"), "late·A"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			c.setup(t)
			out, ok := runPool(t, poolOptions{Jobs: c.jobs}, acctTask(t, c.id, c.acct, c.slow), acctTask(t, "free", "free·B", okRunner("free")))
			if !ok {
				t.Fatalf("batch failed: %s", out)
			}
			if first := firstLine(out); !strings.Contains(first, `"task_id":"free"`) {
				t.Errorf("first envelope = %s, want the free-account task to finish first", first)
			}
		})
	}
}

// A dispatch that panics after streaming output is terminal: the fallback dispatch never runs and repeats its work.
func TestBatchPanickingDispatchDoesNotFallBack(t *testing.T) {
	var runs atomic.Int32
	task := chainTask(t, "t", "boom", "boom",
		agent.Dispatch{Agent: boomRunner("boom"), Account: "boom·A"}, agent.Dispatch{Agent: countRunner("next", &runs), Account: "next·B"})
	out, ok := runPool(t, poolOptions{Jobs: 2}, task)
	if ok {
		t.Fatal("batch reported ok after a panicking dispatch")
	}
	if runs.Load() != 0 || !strings.Contains(out, "panic: boom") {
		t.Fatalf("fallback dispatch ran %d times after a panic, want 0:\n%s", runs.Load(), out)
	}
}

// A fallback dispatch that panics is still reported as the fallback, with the first dispatch's failure as the reason.
func TestBatchPanickingFallbackDispatchIsReported(t *testing.T) {
	authEnv(t, `{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"AGAUTH_UNSET"},"models":{"opus":"m"}}`)
	failErr := fmt.Errorf("agauth: %w", providers.ErrNeedsLogin)
	first := runner("claude", func(_ context.Context, w wire.Sink) (wire.Result, error) {
		w.Emit(wire.Event{Type: wire.EventText})
		return wire.Result{}, failErr
	})
	task := chainTask(t, "t", "claude", "claude",
		agent.Dispatch{Agent: first, Account: "claude:agauth"}, agent.Dispatch{Agent: boomRunner("boom"), Account: "boom·B"})
	var out strings.Builder
	runTasks(context.Background(), []batchTask{task}, poolOptions{Jobs: 2, Out: &out})
	var env wire.Envelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &env); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if env.Fallback == nil || env.FallbackReason == "" || len(env.Hops) != 2 {
		t.Fatalf("fallback=%v reason=%q hops=%d, want the panicking dispatch reported as the fallback", env.Fallback, env.FallbackReason, len(env.Hops))
	}
}

// A dispatch whose account another task marked auth-dead while it waited for a permit does not run: it is an auth hop.
func TestQueuedDispatchSkipsAccountMarkedAuthDead(t *testing.T) {
	var runs atomic.Int32
	out, _ := runPool(t, poolOptions{Jobs: 1},
		acctTask(t, "first", "auth·A", markRunner("first", markAuth("auth·A"))),
		acctTask(t, "second", "auth·A", countRunner("second", &runs)))
	if runs.Load() != 0 {
		t.Errorf("the queued dispatch ran %d times on an auth-dead account", runs.Load())
	}
	if ln := envLine(out, "second"); ln != "" && !strings.Contains(ln, `"kind":"auth"`) {
		t.Errorf("second task's envelope = %s, want an auth error", ln)
	}
}

// A retry resumes after the backoff only if its account is still usable: one marked auth-dead meanwhile is skipped.
func TestRetryRechecksAccountAfterBackoff(t *testing.T) {
	var n atomic.Int32
	out, _ := runPool(t, poolOptions{Jobs: 1},
		acctTask(t, "limited", "rate·A", rateOnceRunner("limited", &n)),
		acctTask(t, "marker", "free·B", markRunner("marker", markAuth("rate·A"))))
	if n.Load() != 1 {
		t.Errorf("the rate-limited dispatch ran %d times, want 1: its retry must not run on an auth-dead account", n.Load())
	}
	if ln := envLine(out, "limited"); ln != "" && !strings.Contains(ln, `"kind":"auth"`) {
		t.Errorf("limited task's envelope = %s, want an auth error", ln)
	}
}

// A cooldown that idles the dispatch before attempt 1 is followed by a recheck: an auth-dead account is not attempted.
func TestFirstAttemptRechecksAccountAfterCooldownIdle(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	coolOnFirstLoad(t, "late·A", 200*time.Millisecond)
	var runs atomic.Int32
	runPool(t, poolOptions{Jobs: 1},
		acctTask(t, "late", "late·A", countRunner("late", &runs)),
		acctTask(t, "marker", "free·B", markRunner("marker", markAuth("late·A"))))
	if runs.Load() != 0 {
		t.Errorf("the first attempt ran %d times on an account marked auth-dead during its cooldown wait", runs.Load())
	}
}

// runTasks writes one shared marker per running dispatch and removes every marker when the batch is done.
func TestRunBatchMarkersAppearAndClear(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const account = "shared·acct"
	var cur, runPeak atomic.Int32
	var tasks []batchTask
	for _, name := range []string{"a", "b", "c"} {
		r := gaugeRunner(name, &cur, &runPeak, 60*time.Millisecond, nil)
		tasks = append(tasks, chainTask(t, name, name, name, keepCaller(agent.Dispatch{Agent: r, Account: account})))
	}
	stop := make(chan struct{})
	var peak atomic.Int32
	go samplePeak(inflight.Dir(), account, 2*time.Millisecond, stop, &peak)
	ok := runTasks(context.Background(), tasks, poolOptions{Jobs: 3})
	close(stop)
	if !ok {
		t.Fatal("batch failed")
	}
	if peak.Load() == 0 {
		t.Error("no inflight markers observed while the batch ran")
	}
	if n := hosttest.CountFiles(inflight.Dir(), inflight.Prefix, account); n != 0 {
		t.Errorf("%d markers remain after runTasks returned, want 0", n)
	}
}

// samplePeak counts account markers every interval until stop closes and returns the peak.
func samplePeak(dir, account string, interval time.Duration, stop <-chan struct{}, peak *atomic.Int32) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		n := int32(hosttest.CountFiles(dir, inflight.Prefix, account))
		for cur := peak.Load(); n > cur && !peak.CompareAndSwap(cur, n); cur = peak.Load() {
		}
		time.Sleep(interval)
	}
}

// authEnv seeds a default provider row in a keyring-backed vault.
func authEnv(t *testing.T, row string) {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"agauth","providers":{"anthropic":{"agauth":`+row+`}}}`)
}
