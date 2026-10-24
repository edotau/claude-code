package vendoragent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
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
func tailRecord(t *testing.T, cwd, logPath string, body []byte, status string, pid int) agent.Record {
	t.Helper()
	if body != nil {
		if err := os.WriteFile(logPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rec := agent.NewRecord("codex", agent.Request{Prompt: "tail me"}, time.Now())
	rec.Repo, rec.Log, rec.Status, rec.PID = cwd, logPath, status, pid
	return fixtureRecord(t, rec)
}

// finishLater flips rec to finished ok after d, reporting a Save failure via the returned channel.
func finishLater(rec agent.Record, d time.Duration) <-chan error {
	done := make(chan error, 1)
	go func() {
		time.Sleep(d)
		rec.Finish(agent.Result{}, nil, d)
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
	saved := finishLater(rec, 150*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out syncBuffer
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
		rec agent.Record
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
