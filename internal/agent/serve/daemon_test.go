package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func TestRecoveryBootTime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("daemon is unsupported on Windows")
	}
	boot, err := recoveryBootTime()
	if err != nil || !boot.After(time.Unix(0, 0)) || boot.After(time.Now()) {
		t.Fatalf("boot time = %v, %v", boot, err)
	}
}

func TestConnectionSlotCoversStatusProcessing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket transport")
	}
	root, err := os.MkdirTemp("/tmp", "sv-slot-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	ln, err := listen()
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
	q := NewQueue(Sources{Running: func() map[string][]string {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	}}, 1)
	d := &daemon{q: q, conns: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { d.accept(ctx, ln, &wg) })
	defer func() { close(release); cancel(); _ = ln.Close(); wg.Wait() }()
	first := statusConnection(t)
	defer first.Close()
	if err := writeRequest(first, request{Schema: wireSchema, Op: "status"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("status did not reach queue scan")
	}
	second := statusConnection(t)
	defer second.Close()
	_ = writeRequest(second, request{Schema: wireSchema, Op: "status"}) // refusal may close before the write
	var r reply
	if err := json.NewDecoder(second).Decode(&r); err == nil {
		t.Fatal("extra status request entered a full daemon")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("extra handler waited behind the queue mutex instead of closing")
	}
	if len(d.conns) != 1 {
		t.Fatal("status processing released its lifetime slot")
	}
}

func TestAdmitRefusedWhenWaiterSlotsFullFreesConnSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket transport")
	}
	root, err := os.MkdirTemp("/tmp", "sv-wait-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	ln, err := listen()
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{q: NewQueue(Sources{Running: func() map[string][]string { return nil }}, 1),
		conns: make(chan struct{}, 1), waiters: make(chan struct{}, 1)}
	d.waiters <- struct{}{} // one admit already waiting
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { d.accept(ctx, ln, &wg) })
	defer func() { cancel(); _ = ln.Close(); wg.Wait() }()
	for _, rq := range []request{{Schema: wireSchema, Op: "admit", ID: "x"}, {Schema: wireSchema, Op: "status"}} {
		c := statusConnection(t)
		if err := writeRequest(c, rq); err != nil {
			t.Fatal(err)
		}
		var r reply
		if err := json.NewDecoder(c).Decode(&r); err != nil {
			t.Fatalf("%s: %v", rq.Op, err)
		}
		_ = c.Close()
		if want := map[string]string{"admit": StateRefused, "status": "status"}[rq.Op]; r.State != want {
			t.Fatalf("%s got %q, want %q", rq.Op, r.State, want)
		}
		if rq.Op == "admit" && !strings.Contains(r.Reason, "waiter") {
			t.Fatalf("admit refused for %q, not the full waiter slots", r.Reason)
		}
	}
}

func TestLockOwnerNeedsTheServeDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file guards")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := ensureDir(Dir()); err != nil {
		t.Fatal(err)
	}
	if _, ok := lockOwner(); ok {
		t.Fatal("an unheld lock named an owner")
	}
	lock, err := openServeFile("daemon.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if !paths.TryLock(lock) {
		t.Fatal("lock")
	}
	st := State{PID: os.Getpid(), Started: time.Now().UTC()}
	if err := writeLockOwner(lock, st); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(Dir(), "daemon.lock"))
	if err != nil || string(b) != fmt.Sprintf("%d %d\n", st.PID, st.Started.UnixNano()) {
		t.Fatalf("lock content %q, %v", b, err)
	}
	if _, ok := lockOwner(); ok {
		t.Fatal("the test runner was taken for the serve daemon")
	}
}

func statusConnection(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("unix", SockPath(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStopRefusesNonDaemonPID(t *testing.T) {
	if err := stop(State{PID: os.Getpid()}); err == nil {
		t.Fatal("stop accepted the test runner as the daemon")
	}
}
