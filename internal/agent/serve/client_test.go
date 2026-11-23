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
)

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
	requeueWindow, dialStep = 40*time.Millisecond, time.Millisecond
	t.Cleanup(func() { requeueWindow, dialStep = oldWindow, oldStep })
	if err := ensureDir(Dir()); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"silent", StateQueued, StateRequeue, "drop"} {
		t.Run(state, func(t *testing.T) {
			ln, err := listenPrivate(SockPath())
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var admits atomic.Int32
			wg.Go(func() { serveAdmissionFixture(ln, state, &wg, &admits) })
			defer func() { _ = ln.Close(); wg.Wait() }()
			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 5*requeueWindow)
			defer cancel()
			got := Admit(ctx, agent.Record{ID: "20261122T120000.000-claude"}, io.Discard)
			if elapsed := time.Since(start); got != Direct || elapsed > 6*requeueWindow {
				t.Fatalf("Admit = %s after %s, want Direct within the caller's %s", got, elapsed, 5*requeueWindow)
			}
			if n := admits.Load(); state == "drop" && n != 1 { // a close before any reply is a full daemon
				t.Fatalf("drop before any reply re-asked: %d admits, want 1", n)
			}
		})
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

func serveAdmissionFixture(ln *net.UnixListener, state string, wg *sync.WaitGroup, admits *atomic.Int32) {
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		wg.Go(func() {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			var rq request
			if json.NewDecoder(c).Decode(&rq) != nil {
				return
			}
			if rq.Op == "status" {
				send(c, reply{State: "status", Status: &Status{PID: os.Getpid()}})
				return
			}
			admits.Add(1)
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
