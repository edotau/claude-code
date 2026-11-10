package job

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

type tailOutcome struct {
	rec Record
	err error
}

type resultNoticeWriter struct {
	out     syncBuffer
	results chan struct{}
}

func (w *resultNoticeWriter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	if strings.Contains(string(p), `"type":"result"`) {
		w.results <- struct{}{}
	}
	return n, err
}

func awaitTailResult(t *testing.T, results <-chan struct{}, done <-chan tailOutcome) {
	t.Helper()
	select {
	case <-results:
	case got := <-done:
		t.Fatalf("tail ended before the next turn result: status=%s err=%v", got.rec.Status, got.err)
	case <-time.After(3 * time.Second):
		t.Fatal("tail did not copy the turn result")
	}
}

func TestTailEventsFollowsTwoLiveTurns(t *testing.T) {
	liveEnv(t)
	fastTail(t)
	ctx, cancel := context.WithCancel(context.Background())
	id, wait := startHolder(t, ctx, time.Minute)
	t.Cleanup(func() { cancel(); _ = wait() })
	rec, err := LoadRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	sendTailTurn(t, id, "remember 42", "ok")
	out := &resultNoticeWriter{results: make(chan struct{}, 4)}
	tailCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	done := make(chan tailOutcome, 1)
	go func() {
		got, err := TailEvents(tailCtx, rec, true, out)
		done <- tailOutcome{got, err}
	}()
	awaitTailResult(t, out.results, done)
	sendTailTurn(t, id, "what number", "42 pid ")
	awaitTailResult(t, out.results, done)
	select {
	case got := <-done:
		t.Fatalf("healthy live session lost after a turn: status=%s err=%v", got.rec.Status, got.err)
	case <-time.After(3 * tailPoll):
	}
	stop()
	got := <-done
	if !errors.Is(got.err, context.Canceled) || !got.rec.Alive() {
		t.Fatalf("tail cancellation = %s, %v; want a still-live record and cancellation", got.rec.Status, got.err)
	}
	if n := strings.Count(out.out.String(), `"type":"result"`); n != 2 {
		t.Fatalf("copied %d turn results, want 2", n)
	}
}

func sendTailTurn(t *testing.T, id, prompt, answer string) {
	t.Helper()
	env, err := SendLive(context.Background(), id, prompt, wire.Discard)
	if err != nil || env.Status != wire.StatusOK || !strings.HasPrefix(env.Answer, answer) {
		t.Fatalf("live turn = %+v, %v; want answer starting %q", env, err, answer)
	}
}

func tailEventFixture(t *testing.T, body string) Record {
	t.Helper()
	rec := NewRecord("claude", agent.RunOptions{}, time.Now())
	rec.PID = os.Getpid()
	rec = fixtureRecord(t, rec)
	if err := os.WriteFile(rec.EventsPath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestTailEventsWaitsForSavedResultRecord(t *testing.T) {
	setConfigDir(t)
	fastTail(t)
	const result = "{\"type\":\"result\",\"status\":\"ok\"}\n"
	rec := tailEventFixture(t, result)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out := &resultNoticeWriter{results: make(chan struct{}, 4)}
	done := make(chan tailOutcome, 1)
	go func() {
		got, err := TailEvents(ctx, rec, true, out)
		done <- tailOutcome{got, err}
	}()
	awaitTailResult(t, out.results, done)
	select {
	case got := <-done:
		t.Fatalf("result returned before Save: status=%s err=%v", got.rec.Status, got.err)
	case <-time.After(3 * tailPoll):
	}
	rec.Finish(wire.Result{}, nil, time.Second)
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.rec.Status != "ok" || got.rec.Alive() {
		t.Fatalf("result after Save returned status=%s alive=%v err=%v; want finished ok", got.rec.Status, got.rec.Alive(), got.err)
	}
	if !strings.Contains(out.out.String(), result) {
		t.Fatalf("tail omitted result: %q", out.out.String())
	}
}

type partialErrorWriter struct {
	err error
}

func (w partialErrorWriter) Write(p []byte) (int, error) { return len(p) / 2, w.err }

func TestTailHeartbeatWriteErrors(t *testing.T) {
	t.Run("failed_write_preserves_lastBeat", assertFailedHeartbeatPreservesLastBeat)
	for _, mode := range []string{"log", "events"} {
		t.Run(mode, func(t *testing.T) {
			setConfigDir(t)
			fastTail(t)
			rec := tailEventFixture(t, "")
			if err := os.WriteFile(rec.LogPath(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			want := errors.New("heartbeat output closed")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var err error
			if mode == "events" {
				_, err = TailEvents(ctx, rec, true, partialErrorWriter{want})
			} else {
				_, err = Tail(ctx, rec.Agent, "", rec.ID, true, partialErrorWriter{want})
			}
			if !errors.Is(err, want) {
				t.Fatalf("heartbeat error = %v, want %v", err, want)
			}
		})
	}
}

func assertFailedHeartbeatPreservesLastBeat(t *testing.T) {
	t.Helper()
	fastTail(t)
	lastBeat := time.Now().Add(-2 * tailBeat)
	want := errors.New("heartbeat output closed")
	rec := Record{ID: "heartbeat", Time: lastBeat}
	got, err := eventHeartbeat(partialErrorWriter{want}, rec, 0, lastBeat, lastBeat)
	if !errors.Is(err, want) {
		t.Fatalf("heartbeat error = %v, want %v", err, want)
	}
	if !got.Equal(lastBeat) {
		t.Fatalf("failed heartbeat advanced lastBeat from %s to %s", lastBeat, got)
	}
}
