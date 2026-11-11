package job

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// syncBuffer is a bytes.Buffer safe for Tail writing while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fastTail shrinks the heartbeat and poll intervals for t.
func fastTail(t *testing.T) {
	t.Helper()
	beat, poll := tailBeat, tailPoll
	tailBeat, tailPoll = 30*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { tailBeat, tailPoll = beat, poll })
}

// tailRecord saves a codex record in cwd with a log at logPath holding body (no file when body is nil).
func tailRecord(t *testing.T, cwd, logPath string, body []byte, status string, pid int) Record {
	t.Helper()
	if body != nil {
		if err := os.WriteFile(logPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec := NewRecord("codex", agent.RunOptions{Prompt: "tail me"}, time.Now())
	rec.Repo, rec.Log, rec.Status, rec.PID = cwd, logPath, status, pid
	return fixtureRecord(t, rec)
}

// finishLater flips rec to finished ok after d, reporting a Save failure via the returned channel.
func finishLater(rec Record, d time.Duration) <-chan error {
	done := make(chan error, 1)
	go func() {
		time.Sleep(d)
		rec.Finish(wire.Result{}, nil, d)
		done <- rec.Save()
	}()
	return done
}

// TestTailFinishedOK: a non-follow tail of a finished job prints the log and returns its ok record.
func TestTailFinishedOK(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	rec := tailRecord(t, cwd, filepath.Join(cwd, "job.log"), []byte("line one\nline two\n"), "ok", 0)

	var out bytes.Buffer
	got, err := Tail(context.Background(), "codex", cwd, "", false, &out)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if out.String() != "line one\nline two\n" {
		t.Errorf("Tail output = %q, want the log", out.String())
	}
	if got.ID != rec.ID || got.Status != "ok" {
		t.Errorf("Tail record = %s/%s, want %s/ok", got.ID, got.Status, rec.ID)
	}
}

// TestTailFinishedError: a failed job's record comes back with Status error for the CLI to map to an exit code.
func TestTailFinishedError(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	tailRecord(t, cwd, filepath.Join(cwd, "job.log"), []byte("boom\n"), "error", 0)

	var out bytes.Buffer
	got, err := Tail(context.Background(), "codex", cwd, "", false, &out)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if got.Status != "error" || out.String() != "boom\n" {
		t.Errorf("Tail = %q / status %q, want \"boom\\n\" / error", out.String(), got.Status)
	}
}

// TestTailFollowSilentHeartbeat: a silent running job prints heartbeats, and Tail returns once it finishes.
func TestTailFollowSilentHeartbeat(t *testing.T) {
	setConfigDir(t)
	fastTail(t)
	cwd := t.TempDir()
	rec := tailRecord(t, cwd, filepath.Join(cwd, "job.log"), []byte{}, "running", os.Getpid())
	var out syncBuffer
	saved := make(chan error, 1)
	go func() { // finish only once a heartbeat printed: a fixed delay races it under suite load
		for end := time.Now().Add(4 * time.Second); !strings.Contains(out.String(), "no output for") && time.Now().Before(end); {
			time.Sleep(10 * time.Millisecond)
		}
		rec.Finish(wire.Result{}, nil, time.Second)
		saved <- rec.Save()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Tail(ctx, "codex", cwd, "", true, &out)
	if err != nil {
		t.Fatalf("Tail follow: %v (output %q)", err, out.String())
	}
	if err := <-saved; err != nil {
		t.Fatalf("finish Save: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("Tail record status = %q, want ok", got.Status)
	}
	if s := out.String(); !strings.Contains(s, "no output for") || !strings.Contains(s, "(elapsed") {
		t.Errorf("Tail output = %q, want a heartbeat with \"no output for\" and \"(elapsed\"", s)
	}
}

// TestTailFollowLateLines: lines appended while the job runs reach the follower.
func TestTailFollowLateLines(t *testing.T) {
	setConfigDir(t)
	fastTail(t)
	cwd := t.TempDir()
	logPath := filepath.Join(cwd, "job.log")
	rec := tailRecord(t, cwd, logPath, []byte("early\n"), "running", os.Getpid())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out syncBuffer
	type tailRes struct {
		rec Record
		err error
	}
	res := make(chan tailRes, 1)
	go func() {
		r, err := Tail(ctx, "codex", cwd, "", true, &out)
		res <- tailRes{r, err}
	}()

	time.Sleep(50 * time.Millisecond)
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("late one\n")
	time.Sleep(50 * time.Millisecond)
	_, _ = f.WriteString("late two\n")
	_ = f.Close()
	if err := <-finishLater(rec, 50*time.Millisecond); err != nil {
		t.Fatalf("finish Save: %v", err)
	}

	r := <-res
	if r.err != nil {
		t.Fatalf("Tail follow: %v", r.err)
	}
	if r.rec.Status != "ok" {
		t.Errorf("Tail record status = %q, want ok", r.rec.Status)
	}
	s := out.String()
	for _, want := range []string{"early\n", "late one\n", "late two\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("Tail output %q missing %q", s, want)
		}
	}
}

// TestTailNoLogErrors: a job without a log file errors with "no log" when not following.
func TestTailNoLogErrors(t *testing.T) {
	setConfigDir(t)
	cwd := t.TempDir()
	tailRecord(t, cwd, filepath.Join(cwd, "missing.log"), nil, "ok", 0)

	_, err := Tail(context.Background(), "codex", cwd, "", false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no log") {
		t.Errorf("Tail(no log) err = %v, want it to mention \"no log\"", err)
	}
}

// TestTailEventsStopsWhenRecordFinishesWithoutResult: follow drains and returns once the record leaves running.
func TestTailEventsStopsWhenRecordFinishesWithoutResult(t *testing.T) {
	setConfigDir(t)
	fastTail(t)
	rec := NewRecord("claude", agent.RunOptions{Prompt: "x"}, time.Now())
	rec.PID = os.Getpid()
	rec = fixtureRecord(t, rec)
	line := `{"schema":1,"seq":0,"t_ms":0,"type":"text","text":"partial"}` + "\n"
	if err := os.WriteFile(rec.EventsPath(), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	done := finishLater(rec, 60*time.Millisecond)

	var out syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := TailEvents(ctx, rec, true, &out)
	if err != nil {
		t.Fatalf("TailEvents: %v", err)
	}
	if serr := <-done; serr != nil {
		t.Fatal(serr)
	}
	if got.Status != "ok" || !strings.Contains(out.String(), "partial") {
		t.Errorf("status %q out %q, want ok and the event copied", got.Status, out.String())
	}
}

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
