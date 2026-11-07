package ask

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// vendorHost isolates the config dir, seeds a provider and installs a fake opencode that fails on a FAILME prompt.
func vendorHost(t *testing.T) {
	t.Helper()
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
	prepare := func(context.Context) (*agent.Prepared, error) {
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
			Prepare: func(context.Context) (*agent.Prepared, error) {
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
		{ID: "a", Agent: "x", Prepare: func(context.Context) (*agent.Prepared, error) {
			return nil, errors.New("boom a")
		}},
		{ID: "b", Agent: "x", Prepare: func(context.Context) (*agent.Prepared, error) {
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
func checkPrepareEnvelopes(t *testing.T, envs []agent.Envelope, failFast bool) {
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

func checkPrepareEnvelope(t *testing.T, env agent.Envelope, msg string) {
	t.Helper()
	if env.BatchID != "batch-t" || env.Status != "error" || env.Agent != "x" || env.Error == nil {
		t.Fatalf("incorrect prepare failure envelope: %+v", env)
	}
	if !strings.Contains(env.Error.Message, msg) {
		t.Errorf("task %q error message missing %q: %+v", env.TaskID, msg, env.Error)
	}
}

func decodePoolEnvelopes(t *testing.T, r io.Reader) []agent.Envelope {
	t.Helper()
	var envs []agent.Envelope
	dec := json.NewDecoder(r)
	for {
		var env agent.Envelope
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
		return Task{ID: id, Agent: "codex", Plan: plan, Prepare: func(context.Context) (*agent.Prepared, error) {
			rec := agent.NewRecord("codex", agent.RunOptions{Prompt: id}, base.Add(time.Duration(off)*time.Second))
			return &agent.Prepared{Record: rec}, nil
		}}
	}
	var buf bytes.Buffer
	RunTasks(t.Context(), []Task{mk("a", "Phase 1.1", 0), mk("b", "", 1)}, PoolOptions{ID: "batch-x", Jobs: 1, Out: &buf})
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
		if env.BatchID != "batch-m" || env.Status != agent.StatusOK {
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
func runOpts(t *testing.T, initial string, o Opts) map[string][]agent.Envelope {
	t.Helper()
	lines, err := ReadTaskLines(strings.NewReader(initial))
	if err != nil {
		t.Fatal(err)
	}
	o.Jobs, o.Agent, o.Fallback = 1, "opencode", router.FallbackAgents
	base := agent.RunOptions{Provider: "acme", Model: "acme-large", WorkDir: t.TempDir()}
	var buf bytes.Buffer
	id, _ := RunBatch(t.Context(), lines, o, base, false, func() *agent.Client { return agent.NewClient(nil, nil, nil) }, &buf)
	byID := map[string][]agent.Envelope{}
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
		if len(got[id]) != 1 || got[id][0].Status != agent.StatusOK {
			t.Errorf("task %s = %+v, want one ok envelope", id, got[id])
		}
	}
	for _, id := range []string{"u", "np", "t4"} { // the malformed line has no id: it is named after its line number
		if len(got[id]) != 1 || got[id][0].Status != agent.StatusError {
			t.Errorf("bad line %s = %+v, want one error envelope", id, got[id])
		}
	}
	var ok, failed int // the initial a runs; the followed duplicate a is a failure envelope under the same id
	for _, env := range got["a"] {
		if env.Status == agent.StatusOK {
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
	if len(got["a"]) != 1 || got["a"][0].Status != agent.StatusOK || len(got["a"][0].Hops) < 2 || again.Load() != 1 {
		t.Errorf("a = %+v retries %d, want one ok envelope with >= 2 hops after one retry", got["a"], again.Load())
	}
}

// With neither hook set RunBatch is the plain pool: one task, one envelope, no retry.
func TestRunWithoutHooks(t *testing.T) {
	vendorHost(t)
	got := runOpts(t, `{"id":"a","prompt":"FAILME"}`+"\n"+`{"id":"b","prompt":"ok"}`+"\n", Opts{})
	if len(got["a"]) != 1 || got["a"][0].Status != agent.StatusError || len(got["a"][0].Hops) != 1 ||
		len(got["b"]) != 1 || got["b"][0].Status != agent.StatusOK {
		t.Errorf("envelopes = %+v, want a failed once without retry and b ok", got)
	}
}
