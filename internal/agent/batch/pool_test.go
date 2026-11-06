package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

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
