package serve

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/platform/hosttest"
)

// A test that forgets its CLAUDE_CONFIG_DIR still lands in hosttest's dir, never the live daemon socket.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

func TestAdmitRunsDirectWithoutDaemon(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	start := time.Now()
	got := Admit(context.Background(), agent.Record{ID: "20261122T120000.000-claude"}, io.Discard)
	if got != Direct {
		t.Fatalf("Admit with no daemon = %s, want %s", got, Direct)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Admit with no daemon took %s; it must fall back at once", d)
	}
}

func TestAdmitBoundsWaitAndRetries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket transport")
	}
	root, err := os.MkdirTemp("/tmp", "sv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	oldWindow, oldStep := requeueWindow, dialStep
	dialStep = time.Millisecond
	t.Cleanup(func() { requeueWindow, dialStep = oldWindow, oldStep })
	if err := ensureDir(Dir()); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"silent", StateQueued, StateRequeue, "drop"} {
		t.Run(state, func(t *testing.T) { runAdmissionBoundCase(t, state) })
	}
}

func runAdmissionBoundCase(t *testing.T, state string) {
	// Only the silent handshake needs a short deadline.
	requeueWindow = 3 * time.Second
	if state == "silent" {
		requeueWindow = 40 * time.Millisecond
	}
	ln, err := listenPrivate(SockPath())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var admits atomic.Int32
	retried := make(chan struct{})
	wg.Go(func() { serveAdmissionFixture(ln, state, &wg, &admits, retried) })
	defer func() { _ = ln.Close(); wg.Wait() }()

	// This caller deadline only guards against a hung test.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	queued := make(chan struct{}, 1)
	note := admissionEventNote(state, queued)
	finished := make(chan Outcome, 1)
	go func() {
		finished <- Admit(ctx, agent.Record{ID: "20261122T120000.000-claude"}, note)
	}()
	joined := false
	defer joinAdmission(t, cancel, finished, &joined)
	awaitAdmissionEvent(t, state, ctx, queued, retried, cancel)
	got := awaitAdmissionResult(t, state, finished)
	joined = true
	if got != Direct {
		t.Fatalf("Admit = %s, want Direct", got)
	}
	if (state == "silent" || state == "drop") && ctx.Err() != nil {
		t.Fatalf("Admit needed caller cancellation (%s)", state)
	}
	if n := admits.Load(); state == "drop" && n != 1 { // a close before any reply is a full daemon
		t.Fatalf("drop before any reply re-asked: %d admits, want 1", n)
	}
}

func admissionEventNote(state string, queued chan<- struct{}) io.Writer {
	if state == StateQueued || state == StateRequeue {
		return admissionNote(func() { queued <- struct{}{} })
	}
	return io.Discard
}

func joinAdmission(t *testing.T, cancel context.CancelFunc, finished <-chan Outcome, joined *bool) {
	cancel()
	if !*joined {
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			t.Error("Admit goroutine did not stop after cancellation")
		}
	}
}

func awaitAdmissionResult(t *testing.T, state string, finished <-chan Outcome) Outcome {
	t.Helper()
	select {
	case got := <-finished:
		return got
	case <-time.After(10 * time.Second):
		t.Fatalf("Admit did not return after handshake deadline or cancellation (%s)", state)
		return ""
	}
}

func awaitAdmissionEvent(t *testing.T, state string, ctx context.Context, queued, retried <-chan struct{}, cancel context.CancelFunc) {
	t.Helper()
	if state == StateRequeue {
		select {
		case <-retried: // the replacement daemon received a second admit
		case <-ctx.Done():
			t.Fatal("Admit did not retry after requeue")
		}
	}
	if state == StateQueued || state == StateRequeue {
		select {
		case <-queued: // client processed queued; handshake deadline is cleared
			cancel()
		case <-ctx.Done():
			t.Fatal("Admit did not process the queued reply")
		}
	}
}

func TestAdmitWaitsPastHandshakeWindow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket transport")
	}
	root, err := os.MkdirTemp("/tmp", "sv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	oldWindow := requeueWindow
	requeueWindow = 40 * time.Millisecond
	t.Cleanup(func() { requeueWindow = oldWindow })
	if err := ensureDir(Dir()); err != nil {
		t.Fatal(err)
	}
	ln, err := listenPrivate(SockPath())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		c, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer c.Close()
		var rq request
		if json.NewDecoder(c).Decode(&rq) != nil {
			return
		}
		send(c, reply{State: StateQueued})
		time.Sleep(3 * requeueWindow)
		send(c, reply{State: StateAdmitted})
	})
	defer func() { _ = ln.Close(); wg.Wait() }()
	start := time.Now()
	got := Admit(context.Background(), agent.Record{ID: "20261122T120000.000-claude"}, io.Discard)
	if elapsed := time.Since(start); got != Admitted || elapsed < 3*requeueWindow {
		t.Fatalf("Admit = %s after %s, want Admitted after the %s queue wait", got, elapsed, 3*requeueWindow)
	}
}

type admissionNote func()

func (f admissionNote) Write(p []byte) (int, error) {
	f()
	return len(p), nil
}

func serveAdmissionFixture(ln *net.UnixListener, state string, wg *sync.WaitGroup, admits *atomic.Int32, retried chan<- struct{}) {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		wg.Go(func() {
			defer c.Close()
			// Keep the fixture open past the handshake and cancellation guards.
			_ = c.SetDeadline(time.Now().Add(30 * time.Second))
			var rq request
			if json.NewDecoder(c).Decode(&rq) != nil {
				return
			}
			if rq.Op == "status" {
				send(c, reply{State: "status", Status: &Status{PID: os.Getpid()}})
				return
			}
			if admits.Add(1) == 2 && state == StateRequeue {
				close(retried)
				// Clear the handshake deadline; wait for cancellation.
				send(c, reply{State: StateQueued})
				_, _ = io.Copy(io.Discard, c)
				return
			}
			switch state {
			case StateRequeue:
				send(c, reply{State: state})
				return
			case "drop":
				return
			case StateQueued:
				send(c, reply{State: state})
			}
			_, _ = io.Copy(io.Discard, c)
		})
	}
}
