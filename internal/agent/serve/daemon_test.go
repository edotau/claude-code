package serve

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

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
