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
	"os/exec"
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
	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/inflight"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
func vendorPrepare(t *testing.T, prompt string, n *atomic.Int32) func(context.Context) (*client.Prepared, error) {
	dir := t.TempDir()
	return func(ctx context.Context) (*client.Prepared, error) {
		n.Add(1)
		return client.NewClient(nil, nil, nil).Prepare(ctx, agent.RunOptions{Agent: "opencode", Provider: "acme",
			Model: "acme-large", Prompt: prompt, WorkDir: dir, Fallback: router.FallbackAgents}, nil)
	}
}

// vendorTask is a task whose first attempt runs the first prompt and whose retry, when given, runs the second.
func vendorTask(t *testing.T, id, plan, first, retry string, prep, again *atomic.Int32) Task {
	task := Task{ID: id, Agent: "opencode", Plan: plan, Prepare: vendorPrepare(t, first, prep)}
	if retry != "" {
		task.Retry = vendorPrepare(t, retry, again)
	}
	return task
}

func TestRunTasksPreparesOverlap(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	prepare := func(context.Context) (*client.Prepared, error) {
		entered <- struct{}{}
		<-release
		return nil, errors.New("prepare failed")
	}
	var buf bytes.Buffer
	done := make(chan bool, 1)
	go func() {
		done <- RunTasks(t.Context(), []Task{
			{ID: "a", Agent: "x", Prepare: prepare},
			{ID: "b", Agent: "x", Prepare: prepare},
		}, PoolOptions{Jobs: 2, Out: &buf})
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case <-entered:
		case <-timer.C:
			t.Fatal("both prepares must enter before either is released")
		}
	}
	close(release)
	select {
	case ok := <-done:
		if ok {
			t.Error("RunTasks returned true for prepare errors")
		}
	case <-timer.C:
		t.Fatal("RunTasks did not finish after releasing prepares")
	}
	if envs := decodePoolEnvelopes(t, &buf); len(envs) != 2 {
		t.Fatalf("got %d envelopes, want 2", len(envs))
	}
}

func TestRunTasksPrepareErrorEnvelope(t *testing.T) {
	var tasks []Task
	for _, id := range []string{"a", "b"} {
		tasks = append(tasks, Task{ID: id, Agent: "x",
			Prepare: func(context.Context) (*client.Prepared, error) {
				return nil, errors.New("boom " + id)
			},
		})
	}
	var buf bytes.Buffer
	if RunTasks(t.Context(), tasks, PoolOptions{ID: "batch-t", Jobs: 1, Out: &buf}) {
		t.Error("RunTasks returned true for prepare errors")
	}
	checkPrepareEnvelopes(t, decodePoolEnvelopes(t, &buf), false)
}

func TestRunTasksPrepareErrorEnvelopeFailFast(t *testing.T) {
	var secondCalls atomic.Int32
	tasks := []Task{
		{ID: "a", Agent: "x", Prepare: func(context.Context) (*client.Prepared, error) {
			return nil, errors.New("boom a")
		}},
		{ID: "b", Agent: "x", Prepare: func(context.Context) (*client.Prepared, error) {
			secondCalls.Add(1)
			return nil, errors.New("boom b")
		}},
	}
	var buf bytes.Buffer
	if RunTasks(t.Context(), tasks, PoolOptions{ID: "batch-t", Jobs: 1, FailFast: true, Out: &buf}) {
		t.Error("RunTasks returned true for a prepare error")
	}
	if n := secondCalls.Load(); n != 0 {
		t.Errorf("second Prepare called %d times, want 0", n)
	}
	checkPrepareEnvelopes(t, decodePoolEnvelopes(t, &buf), true)
}

// checkPrepareEnvelopes asserts one error envelope per task a and b; failFast expects b cancelled before start.
func checkPrepareEnvelopes(t *testing.T, envs []wire.Envelope, failFast bool) {
	t.Helper()
	if len(envs) != 2 {
		t.Fatalf("got %d envelopes, want 2", len(envs))
	}
	want := map[string]string{"a": "boom a", "b": "boom b"}
	if failFast {
		want["b"] = "cancelled before start"
	}
	for _, env := range envs {
		msg, ok := want[env.TaskID]
		if !ok {
			t.Errorf("unexpected or duplicate task id %q", env.TaskID)
			continue
		}
		delete(want, env.TaskID)
		checkPrepareEnvelope(t, env, msg)
	}
}

func checkPrepareEnvelope(t *testing.T, env wire.Envelope, msg string) {
	t.Helper()
	if env.BatchID != "batch-t" || env.Status != "error" || env.Agent != "x" || env.Error == nil {
		t.Fatalf("incorrect prepare failure envelope: %+v", env)
	}
	if !strings.Contains(env.Error.Message, msg) {
		t.Errorf("task %q error message missing %q: %+v", env.TaskID, msg, env.Error)
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
	mk := func(id, plan string, off int) Task {
		return Task{ID: id, Agent: "codex", Plan: plan, Prepare: func(context.Context) (*client.Prepared, error) {
			rec := job.NewRecord("codex", agent.RunOptions{Prompt: id}, base.Add(time.Duration(off)*time.Second))
			return &client.Prepared{Record: rec}, nil
		}}
	}
	var buf bytes.Buffer
	RunTasks(t.Context(), []Task{mk("a", "Phase 1.1", 0), mk("b", "", 1)}, PoolOptions{ID: "batch-x", Jobs: 1, Out: &buf})
	got := map[string]job.Record{}
	for _, r := range job.RecentRecords(10) {
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

// Tasks sent on More run while the batch is open; RunTasks returns only after More closes and every task ends.
func TestRunTasksMoreClaimsTasks(t *testing.T) {
	vendorHost(t)
	var n atomic.Int32
	more := make(chan Task)
	var buf bytes.Buffer
	done := make(chan bool, 1)
	go func() {
		done <- RunTasks(t.Context(), []Task{vendorTask(t, "a", "", "ok", "", &n, &n)}, PoolOptions{ID: "batch-m", Jobs: 1, Out: &buf, More: more})
	}()
	more <- vendorTask(t, "b", "", "ok", "", &n, &n)
	more <- vendorTask(t, "c", "", "ok", "", &n, &n)
	select {
	case <-done:
		t.Fatal("RunTasks returned before More was closed")
	case <-time.After(300 * time.Millisecond):
	}
	close(more)
	select {
	case ok := <-done:
		if !ok {
			t.Error("RunTasks = false, want every task ok")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunTasks did not return after More closed")
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
	more := make(chan Task, 1) // never closed: RunTasks must end on the cancellation alone
	more <- vendorTask(t, "m", "", "ok", "", &prepB, &unused)
	var buf bytes.Buffer
	done := make(chan bool, 1)
	go func() {
		done <- RunTasks(t.Context(), []Task{vendorTask(t, "a", "", "FAILME", "", &prepA, &unused)},
			PoolOptions{ID: "batch-f", Jobs: 1, FailFast: true, Out: &buf, More: more})
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("RunTasks = true, want false after the failure")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunTasks hung with More open under FailFast")
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

// runOpts runs the JSONL initial lines through RunBatch against the fake opencode vendor and returns envelopes by task id.
func runOpts(t *testing.T, initial string, o Opts) map[string][]wire.Envelope {
	t.Helper()
	lines, err := ReadTaskLines(strings.NewReader(initial))
	if err != nil {
		t.Fatal(err)
	}
	o.Jobs, o.Agent, o.Fallback = 1, "opencode", router.FallbackAgents
	base := agent.RunOptions{Provider: "acme", Model: "acme-large", WorkDir: t.TempDir()}
	var buf bytes.Buffer
	id, _ := RunBatch(t.Context(), lines, o, base, false, func() *client.Client { return client.NewClient(nil, nil, nil) }, &buf)
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
func followLines(raw string, firstN int) func(context.Context, func(ParsedLine) Task) <-chan Task {
	return func(_ context.Context, build func(ParsedLine) Task) <-chan Task {
		out := make(chan Task, 16)
		lines, _ := ReadTaskLines(strings.NewReader(raw))
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
	got := runOpts(t, `{"id":"a","prompt":"ok"}`+"\n", Opts{Follow: follow})
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
	decorate := func(task *Task) { task.Retry = vendorPrepare(t, "ok", &again) }
	got := runOpts(t, `{"id":"a","prompt":"FAILME"}`+"\n", Opts{Decorate: decorate})
	if len(got["a"]) != 1 || got["a"][0].Status != wire.StatusOK || len(got["a"][0].Hops) < 2 || again.Load() != 1 {
		t.Errorf("a = %+v retries %d, want one ok envelope with >= 2 hops after one retry", got["a"], again.Load())
	}
}

// With neither hook set RunBatch is the plain pool: one task, one envelope, no retry.
func TestRunWithoutHooks(t *testing.T) {
	vendorHost(t)
	got := runOpts(t, `{"id":"a","prompt":"FAILME"}`+"\n"+`{"id":"b","prompt":"ok"}`+"\n", Opts{})
	if len(got["a"]) != 1 || got["a"][0].Status != wire.StatusError || len(got["a"][0].Hops) != 1 ||
		len(got["b"]) != 1 || got["b"][0].Status != wire.StatusOK {
		t.Errorf("envelopes = %+v, want a failed once without retry and b ok", got)
	}
}

// runRetryBatch runs tasks with one job and returns the ok flag and the decoded envelopes by task id.
func runRetryBatch(t *testing.T, o PoolOptions, tasks ...Task) (bool, map[string][]wire.Envelope) {
	t.Helper()
	var buf bytes.Buffer
	o.Out, o.ID, o.Jobs = &buf, "batch-r", 1
	ok := RunTasks(t.Context(), tasks, o)
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
	_, got := runRetryBatch(t, PoolOptions{}, vendorTask(t, "a", "Phase 1.1", "FAILME", "ok", &prep, &again))
	if len(got["a"]) != 1 || got["a"][0].Status != wire.StatusOK || len(got["a"][0].Hops) < 2 {
		t.Fatalf("envelopes for a = %+v, want one ok envelope with >= 2 hops", got["a"])
	}
	if prep.Load() != 1 || again.Load() != 1 {
		t.Errorf("prepare/retry calls = %d/%d, want 1/1", prep.Load(), again.Load())
	}
	var recs int
	for _, r := range job.RecentRecords(10) {
		recs++
		if r.TaskID != "a" || r.BatchID != "batch-r" || r.PlanTask != "Phase 1.1" {
			t.Errorf("record %s = task %q batch %q plan %q", r.ID, r.TaskID, r.BatchID, r.PlanTask)
		}
	}
	if recs != 2 {
		t.Errorf("%d dispatch records, want 2 (first attempt and retry)", recs)
	}
}

// A retry that fails too is the task's one envelope, and the task never runs a third time.
func TestRetryAlsoFailsEmitsRetryError(t *testing.T) {
	vendorHost(t)
	var prep, again atomic.Int32
	ok, got := runRetryBatch(t, PoolOptions{}, vendorTask(t, "a", "", "FAILME", "FAILME", &prep, &again))
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
		_, got := runRetryBatch(t, PoolOptions{},
			vendorTask(t, "a", "", "FAILME", "ok", &n, &again),
			vendorTask(t, "b", "", "ok", "", &n, &n),
			vendorTask(t, "c", "", "ok", "", &n, &n))
		done <- got
	}()
	select {
	case got := <-done:
		for _, id := range []string{"a", "b", "c"} {
			if len(got[id]) != 1 || got[id][0].Status != wire.StatusOK {
				t.Errorf("task %s envelopes = %+v, want one ok", id, got[id])
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatal("batch deadlocked: a retried task leaked its slot")
	}
}

// No retry without a dispatch (no hops), after a prepare error, after cancellation, or under FailFast.
func TestRetrySkippedWhenNotRealFailure(t *testing.T) {
	vendorHost(t)
	var retried atomic.Int32
	retry := func(context.Context) (*client.Prepared, error) { retried.Add(1); return nil, errors.New("retry ran") }
	noHops := func(context.Context) (*client.Prepared, error) {
		return &client.Prepared{Record: job.NewRecord("opencode", agent.RunOptions{}, time.Now())}, nil
	}
	badPrep := func(context.Context) (*client.Prepared, error) { return nil, errors.New("prepare failed") }
	runRetryBatch(t, PoolOptions{},
		Task{ID: "h", Agent: "opencode", Prepare: noHops, Retry: retry},
		Task{ID: "p", Agent: "opencode", Prepare: badPrep, Retry: retry})
	if retried.Load() != 0 {
		t.Errorf("Retry ran %d times for a no-hop failure and a prepare error, want 0", retried.Load())
	}
	var prep, again atomic.Int32
	_, got := runRetryBatch(t, PoolOptions{FailFast: true},
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
	task.Prepare = func(c context.Context) (*client.Prepared, error) {
		p, err := inner(c)
		cancel()
		return p, err
	}
	var buf bytes.Buffer
	RunTasks(ctx, []Task{task}, PoolOptions{ID: "batch-c", Jobs: 1, Out: &buf})
	if again.Load() != 0 || len(decodePoolEnvelopes(t, &buf)) != 1 {
		t.Errorf("retry calls = %d, want 0 and one envelope", again.Load())
	}
}

// A batch task's own profile reaches the request and resolves to one dispatch, under the default --fallback all.
func TestBatchTaskProfileIsOneDispatch(t *testing.T) {
	profileFixture(t, "")
	line := ParsedLine{N: 1, Task: TaskLine{Prompt: "p", Profile: "acme"}}
	bt, err := BuildTask(line, Opts{Agent: "codex", Fallback: router.FallbackAll}, agent.RunOptions{}, false, t.TempDir(), map[string]bool{}, NewClient)
	if err != nil {
		t.Fatal(err)
	}
	if bt.Req.Provider != "acme" {
		t.Fatalf("task request provider = %q, want acme", bt.Req.Provider)
	}
	p, err := bt.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	res, err := NewClient().Exec(context.Background(), p, nil, job.RunRecordedOpts{})
	if err == nil || len(res.Hops) != 1 {
		t.Fatalf("Exec: %d hops, err %v; want one unavailable dispatch", len(res.Hops), err)
	}
	bad := ParsedLine{N: 2, Task: TaskLine{Prompt: "p", Profile: "no-such-profile"}}
	bt, err = BuildTask(bad, Opts{Agent: "codex", Fallback: router.FallbackAll}, agent.RunOptions{}, false, t.TempDir(), map[string]bool{}, NewClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bt.Prepare(context.Background()); err == nil {
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
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "slow", "agent": "claude", "prompt": "SLOW task", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "fast", "agent": "claude", "prompt": "quick task", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "bad", "agent": "no-such-agent", "prompt": "x", "workdir": t.TempDir()}),
		`{not json`,
	)
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "2")
	if code == 0 {
		t.Fatalf("exit 0 with failing tasks\nstdout: %s\nstderr: %s", out, stderr)
	}
	envs := envelopes(t, out)
	if len(envs) != 4 {
		t.Fatalf("got %d envelopes, want 4 (one per input line, bad lines included)\n%s", len(envs), out)
	}
	byID := indexBatchEnvelopes(t, envs)
	checkMixedBatchResults(t, byID, envs, out)
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
	// completion order: the SLOW task finishes after the fast one
	pos := map[string]int{}
	for i, e := range envs {
		id, _ := e["task_id"].(string)
		pos[id] = i
	}
	if pos["fast"] > pos["slow"] {
		t.Errorf("fast task printed after the slow one: streaming must be completion order\n%s", out)
	}
}

// All-ok batch exits 0.
func TestBatchAllOkExitsZero(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "a", "agent": "claude", "prompt": "one", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "b", "agent": "claude", "prompt": "two", "workdir": t.TempDir(), "tools": true}),
	)
	code, out, stderr := runCLI(t, "ask", "--batch", file)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	for _, e := range envelopes(t, out) {
		if e["status"] != "ok" {
			t.Errorf("envelope %v, want ok", e)
		}
	}
}

// --fail-fast: the first fatal failure cancels the batch; a queued task reports cancelled and never spawns.
func TestBatchFailFastNeverSpawnsQueuedTask(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	pathstest.FakeVendor(t, "claude", "cat > /dev/null; echo boom >&2; exit 1")
	marker := cliMarker(t, "opencode", `printf '%s\n' '{"type":"text","sessionID":"so","part":{"text":"hello"}}'`)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "first", "agent": "claude", "prompt": "fail", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "queued", "agent": "opencode", "prompt": "never", "workdir": t.TempDir(), "tools": true}),
	)
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

func fakeJWT(iss string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"https://`+iss+`"}`)) + ".sig"
}

// Guarantee 1: concurrent tasks on two profiles; each child's env holds only its own route and token.
func TestBatchConcurrentProfilesKeepEnvsApart(t *testing.T) {
	skipWithoutSh(t)
	cliEnv(t)
	vaulttest.Use(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PA_BATCH_KEY", "TOKEN-AAAA-1111")
	t.Setenv("PB_BATCH_KEY", "TOKEN-BBBB-2222")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"pa":{"protocol":"anthropic","base_url":"http://127.0.0.1:11111","auth":{"type":"key","env":"PA_BATCH_KEY"},"models":{"opus":"m-a","sonnet":"m-a","haiku":"m-a"}},`+
		`"pb":{"protocol":"anthropic","base_url":"http://127.0.0.1:22222","auth":{"type":"key","env":"PB_BATCH_KEY"},"models":{"opus":"m-b","sonnet":"m-b","haiku":"m-b"}}}}}`)
	dump := t.TempDir()
	// each child announces itself, then waits until both are running: proof the two really overlap
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\n"+
		"env > "+dump+"/$$.env\ntouch "+dump+"/$$.up\n"+
		"for i in $(seq 1 100); do [ $(ls "+dump+"/*.up 2>/dev/null | wc -l) -ge 2 ] && break; sleep 0.05; done\n"+
		`printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "a", "agent": "claude", "profile": "pa", "prompt": "x", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "b", "agent": "claude", "profile": "pb", "prompt": "y", "workdir": t.TempDir(), "tools": true}),
	)
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
	t.Setenv("HM_BATCH_KEY", fakeJWT("adb-2.azuredatabricks.net"))
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"hm":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net",`+
		`"auth":{"type":"key","env":"HM_BATCH_KEY"},"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5","haiku":"databricks-claude-haiku-4-5"}}}}}`)
	marker := cliMarker(t, "claude", `printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t, taskLine(t, map[string]any{"id": "m", "agent": "claude", "profile": "hm", "prompt": "x", "workdir": t.TempDir(), "tools": true}))
	code, out, stderr := runCLI(t, "ask", "--batch", file)
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

// Every --batch task goes through RunRecorded: it leaves an <id>.events.jsonl running start to result.
func TestBatchTasksWriteEventFiles(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "one", "agent": "claude", "prompt": "p1", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "two", "agent": "claude", "prompt": "p2", "workdir": t.TempDir(), "tools": true}),
	)
	if code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "2"); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
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
	skipWithoutSh(t)
	cliEnv(t)
	vaulttest.Use(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PA_BATCH_KEY", "TOKEN-AAAA-1111")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"pa":{"protocol":"anthropic","base_url":"http://127.0.0.1:11111","auth":{"type":"key","env":"PA_BATCH_KEY"},"models":{"opus":"m-a","sonnet":"m-a","haiku":"m-a"}},`+
		`"pb":{"protocol":"anthropic","base_url":"http://127.0.0.1:22222","auth":{"type":"key","env":"PB_BATCH_KEY"},"models":{"opus":"m-b","sonnet":"m-b","haiku":"m-b"}}}}}`)
	if err := providers.SetCustomEnv("PB_BATCH_KEY", "PB-CUSTOM-SECRET"); err != nil {
		t.Fatal(err)
	}
	if err := providers.SetCustomEnv("PA_CUSTOM_NOTE", "pa-note"); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "child.env")
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\nenv > "+dump+"\n"+`printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t, taskLine(t, map[string]any{"id": "a", "agent": "claude", "profile": "pa", "prompt": "x", "workdir": t.TempDir(), "tools": true}))
	if code, out, stderr := runCLI(t, "ask", "--batch", file); code != 0 {
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

// batchPipeHelper is the child process of TestBatchClosedStdoutCancelsAndReapsChildren: it runs a real batch whose
// stdout reader is gone, with one slow task whose fake CLI records its pid.
func TestBatchPipeHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs unix signals and pipes")
	}
	pidfile := os.Getenv("BATCH_PIPE_PIDFILE")
	if pidfile == "" {
		t.Skip("helper process only")
	}
	cliEnv(t)
	pathstest.FakeVendor(t, "claude", `p=$(cat); case "$p" in *SLOW*) echo $$ > `+pidfile+`; sleep 60;; esac`+"\n"+
		"for i in $(seq 1 100); do [ -s "+pidfile+" ] && break; sleep 0.05; done\n"+
		`printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "slow", "agent": "claude", "prompt": "SLOW", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "fast", "agent": "claude", "prompt": "quick", "workdir": t.TempDir(), "tools": true}))
	os.Exit(Cmd([]string{"--batch", file, "--jobs", "2"}))
}

// A closed stdout must not kill the process with SIGPIPE: the batch cancels, exits non-zero, and its children die.
func TestBatchClosedStdoutCancelsAndReapsChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs unix signals and pipes")
	}
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBatchPipeHelper$")
	cmd.Env = append(os.Environ(), "BATCH_PIPE_PIDFILE="+pidfile)
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close() // nobody reads the batch's stdout any more
	w.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the batch did not exit after its stdout closed")
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signaled() || ws.ExitStatus() == 0 {
		t.Fatalf("exit status %v, want a normal non-zero exit (SIGPIPE must not kill the process)", cmd.ProcessState)
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
	var tasks []Task
	for i := range retry.DefaultPerKey + 3 {
		name := fmt.Sprintf("agent%d", i)
		r := gaugeRunner{name: name, cur: &cur, max: &peak}
		tasks = append(tasks, Task{ID: name, Agent: name, Req: agent.RunOptions{Prompt: "p"},
			Prepare: func(context.Context) (*client.Prepared, error) {
				return setChain(&client.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: job.Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}}, agent.Chain{Want: name, Dispatches: []agent.Dispatch{keepCaller(agent.Dispatch{Agent: r, Account: "shared·acct"})}}), nil
			}})
	}
	var out strings.Builder
	if !RunTasks(context.Background(), tasks, PoolOptions{Jobs: len(tasks), Out: &out}) {
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

// setChain gives a Prepared the routed chain the batch tests dispatch through.
func setChain(p *client.Prepared, c agent.Chain) *client.Prepared { p.Chain = c; return p }

// acctTask is a batch task whose single dispatch bills account.
func acctTask(t *testing.T, name, account string, r agent.Engine) Task {
	return Task{ID: name, Agent: name, Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*client.Prepared, error) {
			rec := job.Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}
			return setChain(&client.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: rec}, agent.Chain{Want: name, Dispatches: []agent.Dispatch{keepCaller(agent.Dispatch{Agent: r, Account: account})}}), nil
		}}
}

// A panicking agent (recovered by the batch) must give its dispatch permit back: later tasks on the account still run.
func TestBatchPanickingDispatchesReleaseTheirPermits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var tasks []Task
	for i := range retry.DefaultPerKey + 2 {
		name := fmt.Sprintf("p%d", i)
		tasks = append(tasks, acctTask(t, name, "panic·acct", slowRunner{name: name, boom: true}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	if RunTasks(ctx, tasks, PoolOptions{Jobs: 2, Out: &out}) {
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
	var tasks []Task
	n := retry.DefaultPerKey + 2
	for i := range n {
		name := fmt.Sprintf("a%d", i)
		tasks = append(tasks, acctTask(t, name, "A·acct", slowRunner{name: name, wait: bRan}))
	}
	tasks = append(tasks, acctTask(t, "b", "B·acct", slowRunner{name: "b", done: bRan}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	if !RunTasks(ctx, tasks, PoolOptions{Jobs: n, Out: &out}) {
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
	var tasks []Task
	for i := range 12 {
		name := fmt.Sprintf("f%d", i)
		first, second := dispatchGauge{name: "codex", fail: true, cur: &cur, max: &peak}, dispatchGauge{name: "gemini", cur: &cur, max: &peak}
		tasks = append(tasks, Task{ID: name, Agent: name, Req: agent.RunOptions{Prompt: "p"}, // distinct keys: the old per-key gate would cap at 4
			Prepare: func(context.Context) (*client.Prepared, error) {
				rec := job.Record{ID: "id-" + name, Agent: "codex", Log: filepath.Join(t.TempDir(), "l.log")}
				return setChain(&client.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: rec}, agent.Chain{Want: "codex", Dispatches: []agent.Dispatch{
					keepCaller(agent.Dispatch{Agent: first, Account: "A·acct"}), {Agent: second, Account: "B·acct"}}}), nil
			}})
	}
	var out strings.Builder
	if !RunTasks(context.Background(), tasks, PoolOptions{Jobs: jobs, Out: &out}) {
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
func failFastPair(t *testing.T, first Task) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var prepared, runs atomic.Int32
	second := acctTask(t, "second", "ff·acct", flagRunner{name: "second", runs: &runs})
	inner := second.Prepare
	second.Prepare = func(ctx context.Context) (*client.Prepared, error) { prepared.Add(1); return inner(ctx) }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out strings.Builder
	if RunTasks(ctx, []Task{first, second}, PoolOptions{Jobs: 1, FailFast: true, Out: &out}) {
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
	failFastPair(t, Task{ID: "first", Agent: "first", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*client.Prepared, error) { return nil, errors.New("route failed") }})
}

// --fail-fast with a panicking dispatch: the next task never spawns.
func TestBatchFailFastPanickingDispatchStopsQueuedTask(t *testing.T) {
	failFastPair(t, acctTask(t, "first", "ff·acct", slowRunner{name: "first", boom: true}))
}

// A task waiting out its account's cooldown holds nothing: a task on another account runs and finishes first.
func TestBatchCoolingAccountDoesNotBlockOthers(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	retry.CoolDown("cool·A", 250*time.Millisecond)
	tasks := []Task{
		acctTask(t, "cooling", "cool·A", slowRunner{name: "cooling"}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !RunTasks(context.Background(), tasks, PoolOptions{Jobs: 2, Out: &out}) {
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
	first := Task{ID: "first", Agent: "first", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*client.Prepared, error) { return nil, errors.New("route failed") }}
	second := acctTask(t, "second", "pf·acct", flagRunner{name: "second", runs: &runs})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	RunTasks(ctx, []Task{first, second}, PoolOptions{Jobs: 1, Out: &out})
	if runs.Load() != 1 {
		t.Fatalf("second task ran %d times after the first task's Prepare failed, want 1:\n%s", runs.Load(), out.String())
	}
}

// A dispatch that panics after streaming output is terminal: the fallback dispatch never runs and repeats its work.
func TestBatchPanickingDispatchDoesNotFallBack(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var runs atomic.Int32
	task := Task{ID: "t", Agent: "boom", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*client.Prepared, error) {
			rec := job.Record{ID: "id-t", Agent: "boom", Log: filepath.Join(t.TempDir(), "l.log")}
			return setChain(&client.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: rec}, agent.Chain{Want: "boom", Dispatches: []agent.Dispatch{
				{Agent: slowRunner{name: "boom", boom: true}, Account: "boom·A"},
				{Agent: flagRunner{name: "next", runs: &runs}, Account: "next·B"},
			}}), nil
		}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out strings.Builder
	if RunTasks(ctx, []Task{task}, PoolOptions{Jobs: 2, Out: &out}) {
		t.Fatal("batch reported ok after a panicking dispatch")
	}
	if runs.Load() != 0 || !strings.Contains(out.String(), "panic: boom") {
		t.Fatalf("fallback dispatch ran %d times after a panic, want 0:\n%s", runs.Load(), out.String())
	}
}

// A fallback dispatch that panics is still reported as the fallback, with the first dispatch's failure as the reason.
func TestBatchPanickingFallbackDispatchIsReported(t *testing.T) {
	authEnv(t, `{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"AGAUTH_UNSET"},"models":{"opus":"m"}}`)
	first := newFailingRunner("claude", fmt.Errorf("agauth: %w", providers.ErrNeedsLogin))
	task := Task{ID: "t", Agent: "claude", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*client.Prepared, error) {
			rec := job.Record{ID: "id-t", Agent: "claude", Log: filepath.Join(t.TempDir(), "l.log")}
			return setChain(&client.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: rec}, agent.Chain{Want: "claude", Dispatches: []agent.Dispatch{
				{Agent: first, Account: "claude:agauth"},
				{Agent: slowRunner{name: "boom", boom: true}, Account: "boom·B"},
			}}), nil
		}}
	var out strings.Builder
	RunTasks(context.Background(), []Task{task}, PoolOptions{Jobs: 2, Out: &out})
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
	failFastPair(t, Task{ID: "first", Agent: "first", Req: agent.RunOptions{Prompt: "p"},
		Prepare: func(context.Context) (*client.Prepared, error) {
			return setChain(&client.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: job.Record{ID: "id-first", Agent: "first", Log: filepath.Join(t.TempDir(), "l.log")}}, agent.Chain{Want: "first"}), nil
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
	tasks := []Task{
		acctTask(t, "limited", "rate·A", rateOnce{name: "limited", n: &n}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !RunTasks(context.Background(), tasks, PoolOptions{Jobs: 1, Out: &out}) {
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
			once.Do(func() { retry.CoolDown("late·A", 200*time.Millisecond) })
			return prev()
		}
	}))
	tasks := []Task{
		acctTask(t, "late", "late·A", slowRunner{name: "late"}),
		acctTask(t, "free", "free·B", slowRunner{name: "free"}),
	}
	var out strings.Builder
	if !RunTasks(context.Background(), tasks, PoolOptions{Jobs: 1, Out: &out}) {
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
	tasks := []Task{
		acctTask(t, "first", "auth·A", first),
		acctTask(t, "second", "auth·A", flagRunner{name: "second", runs: &runs}),
	}
	var out strings.Builder
	RunTasks(context.Background(), tasks, PoolOptions{Jobs: 1, Out: &out})
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
	tasks := []Task{
		acctTask(t, "limited", "rate·A", rateOnce{name: "limited", n: &n}),
		acctTask(t, "marker", "free·B", markingRunner{name: "marker", mark: func() { _ = router.MarkAuth("rate·A", "login expired") }}),
	}
	var out strings.Builder
	RunTasks(context.Background(), tasks, PoolOptions{Jobs: 1, Out: &out})
	if n.Load() != 1 {
		t.Errorf("the rate-limited dispatch ran %d times, want 1: its retry must not run on an auth-dead account", n.Load())
	}
	for ln := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(ln, `"task_id":"limited"`) && !strings.Contains(ln, `"kind":"auth"`) {
			t.Errorf("limited task's envelope = %s, want an auth error", ln)
		}
	}
}

// A cooldown that idles the dispatch before attempt 1 is followed by a recheck: an account marked auth-dead meanwhile
// is not attempted.
func TestFirstAttemptRechecksAccountAfterCooldownIdle(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var once sync.Once
	t.Cleanup(agent.SwapLoadState(func(prev func() router.State) func() router.State {
		return func() router.State {
			once.Do(func() { retry.CoolDown("late·A", 200*time.Millisecond) })
			return prev()
		}
	}))
	var runs atomic.Int32
	tasks := []Task{
		acctTask(t, "late", "late·A", flagRunner{name: "late", runs: &runs}),
		acctTask(t, "marker", "free·B", markingRunner{name: "marker", mark: func() { _ = router.MarkAuth("late·A", "login expired") }}),
	}
	var out strings.Builder
	RunTasks(context.Background(), tasks, PoolOptions{Jobs: 1, Out: &out})
	if runs.Load() != 0 {
		t.Errorf("the first attempt ran %d times on an account marked auth-dead during its cooldown wait", runs.Load())
	}
}

// Own-login dispatches (no account) on one agent still share agent.DefaultPerKey, even when --jobs allows more.
func TestBatchCapsOwnLoginDispatchesPerAgent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var cur, peak atomic.Int32
	r := gaugeRunner{name: "opencode", cur: &cur, max: &peak}
	var tasks []Task
	for i := range retry.DefaultPerKey + 3 {
		id := fmt.Sprintf("own%d", i)
		tasks = append(tasks, Task{ID: id, Agent: "opencode", Req: agent.RunOptions{Prompt: "p"},
			Prepare: func(context.Context) (*client.Prepared, error) {
				return setChain(&client.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: job.Record{ID: "id-" + id, Agent: "opencode", Log: filepath.Join(t.TempDir(), "l.log")}}, agent.Chain{Want: "opencode", Dispatches: []agent.Dispatch{keepCaller(agent.Dispatch{Agent: r})}}), nil
			}})
	}
	var out strings.Builder
	if !RunTasks(context.Background(), tasks, PoolOptions{Jobs: len(tasks), Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > retry.DefaultPerKey || got < 2 {
		t.Errorf("peak concurrent own-login dispatches = %d, want 2..%d", got, retry.DefaultPerKey)
	}
}

// RunTasks writes one shared marker per running dispatch and removes every marker when the batch is done.
func TestRunBatchMarkersAppearAndClear(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const account = "shared·acct"
	var cur, runPeak atomic.Int32
	var tasks []Task
	for _, name := range []string{"a", "b", "c"} {
		r := gaugeRunner{name: name, cur: &cur, max: &runPeak}
		tasks = append(tasks, Task{ID: name, Agent: name, Req: agent.RunOptions{Prompt: "p"},
			Prepare: func(context.Context) (*client.Prepared, error) {
				return setChain(&client.Prepared{Opts: agent.RunOptions{Prompt: "p"}, Record: job.Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}}, agent.Chain{Want: name, Dispatches: []agent.Dispatch{keepCaller(agent.Dispatch{Agent: r, Account: account})}}), nil
			}})
	}
	stop := make(chan struct{})
	var peak atomic.Int32
	go samplePeak(inflight.Dir(), account, 2*time.Millisecond, stop, &peak)

	if !RunTasks(context.Background(), tasks, PoolOptions{Jobs: 3}) {
		t.Fatal("batch failed")
	}
	close(stop)
	if got := peak.Load(); got == 0 {
		t.Error("no inflight markers observed while the batch ran")
	}
	if n := countAccountMarkers(inflight.Dir(), account); n != 0 {
		t.Errorf("%d markers remain after RunTasks returned, want 0", n)
	}
}

// countAccountMarkers counts live marker files in dir whose content is account.
func countAccountMarkers(dir, account string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if _, ok := inflight.Owner(e.Name()); !ok {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && strings.TrimSpace(string(b)) == account {
			n++
		}
	}
	return n
}

// samplePeak counts account markers every interval until stop closes and returns the peak.
func samplePeak(dir, account string, interval time.Duration, stop <-chan struct{}, peak *atomic.Int32) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		n := int32(countAccountMarkers(dir, account))
		for cur := peak.Load(); n > cur && !peak.CompareAndSwap(cur, n); cur = peak.Load() {
		}
		time.Sleep(interval)
	}
}

// keepCaller is d's agent and account on agent.Single's dispatch, which keeps the caller's own --model.
func keepCaller(d agent.Dispatch) agent.Dispatch {
	k := agent.Single(d.Agent).Dispatches[0]
	k.Account = d.Account
	return k
}

// authEnv seeds a default provider row in a keyring-backed vault.
func authEnv(t *testing.T, row string) {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"agauth","providers":{"anthropic":{"agauth":`+row+`}}}`)
}

// failingRunner emits one empty text event, then fails with err and an empty answer.
type failingRunner struct {
	name string
	err  error
}

func (f failingRunner) Name() string     { return f.name }
func (f failingRunner) Caps() agent.Caps { return agent.Caps{} }

func (f failingRunner) Available() error { return nil }
func (f failingRunner) Run(_ context.Context, _ agent.RunOptions, w wire.Sink) (wire.Result, error) {
	w.Emit(wire.Event{Type: wire.EventText})
	return wire.Result{}, f.err
}

// newFailingRunner is a runner whose dispatch fails with err.
func newFailingRunner(name string, err error) agent.Engine {
	return failingRunner{name: name, err: err}
}
