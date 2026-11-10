package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitForSelect(t *testing.T, tid int) {
	t.Helper()
	path := fmt.Sprintf("/proc/self/task/%d/syscall", tid)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(data))
		n, _ := strconv.Atoi(fields[0])
		if n == syscall.SYS_PSELECT6 {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("polling thread never entered select")
}

func TestLinuxARM64TestPackageCompiles(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	tids, done := make(chan int), make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		tids <- syscall.Gettid()
		waitReadable(int(r.Fd()), 100*time.Millisecond)
	}()
	t.Cleanup(func() { <-done })
	waitForSelect(t, <-tids)
	cmd := exec.Command("go", "test", "-c", "-o", filepath.Join(t.TempDir(), "tui.test"), ".")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("linux/arm64 test-package compilation: %v\n%s", err, output)
	}
}

func TestWaitReadableDeadlineSurvivesSignals(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	tids, ready := make(chan int), make(chan bool, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		tids <- syscall.Gettid()
		ready <- waitReadable(int(r.Fd()), 100*time.Millisecond)
	}()
	tid := <-tids
	waitForSelect(t, tid)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(500 * time.Millisecond)
	defer timeout.Stop()
	start := time.Now()
	for {
		select {
		case readable := <-ready:
			if readable || time.Since(start) < 75*time.Millisecond {
				t.Fatalf("interrupted empty pipe: readable=%v after %v; want the original timeout", readable, time.Since(start))
			}
			return
		case <-tick.C:
			if err := syscall.Tgkill(os.Getpid(), tid, syscall.SIGURG); err != nil {
				<-ready
				t.Fatal(err)
			}
		case <-timeout.C:
			<-ready
			t.Fatal("signals extended a 100ms poll beyond 500ms")
		}
	}
}

func TestWaitReadableRetriesInterruptedPoll(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	tids, ready := make(chan int), make(chan bool, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		tids <- syscall.Gettid()
		ready <- waitReadable(int(r.Fd()), time.Second)
	}()
	tid := <-tids
	waitForSelect(t, tid)
	if err := syscall.Tgkill(os.Getpid(), tid, syscall.SIGURG); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := w.WriteString("[200~secret\x1b[201~\n"); err != nil {
		t.Fatal(err)
	}
	if !<-ready {
		t.Fatal("interrupted poll reported a timeout before the paste continuation arrived")
	}
}
