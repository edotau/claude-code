//go:build linux

package tui

import (
	"errors"
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
	"unsafe"
)

func testPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	var unlock, number uint32
	testIoctl(t, master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock))
	testIoctl(t, master, syscall.TIOCGPTN, unsafe.Pointer(&number))
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

func testIoctl(t *testing.T, f *os.File, req uintptr, value unsafe.Pointer) {
	t.Helper()
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(value)); err != 0 {
		t.Fatal(err)
	}
}

func testPickerTTY(t *testing.T, height uint16, input string) {
	t.Helper()
	master, slave := testPTY(t)
	size := [4]uint16{height, 80, 0, 0}
	testIoctl(t, slave, syscall.TIOCSWINSZ, unsafe.Pointer(&size))
	out, err := os.CreateTemp(t.TempDir(), "picker-output")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	stdin, stdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = slave, out
	t.Cleanup(func() { os.Stdin, os.Stdout = stdin, stdout })
	if _, err := master.WriteString(input); err != nil {
		t.Fatal(err)
	}
}

func TestPickEmptyOnTwoLineTerminal(t *testing.T) {
	testPickerTTY(t, 2, "q")
	if idx, err := Pick("", nil, 0); idx != -1 || err != nil {
		t.Fatalf("empty Pick on 80x2 = %d, %v; want -1, nil", idx, err)
	}
}

func TestPickClampsStartPastLastRow(t *testing.T) {
	testPickerTTY(t, 24, "\n")
	rows := [][2]string{{"a", ""}, {"b", ""}, {"c", ""}}
	if idx, err := Pick("", rows, 5); idx != 2 || err != nil {
		t.Fatalf("Pick(start=5) = %d, %v; want last row 2, nil", idx, err)
	}
}

func TestTerminalCloseReportsRestoreError(t *testing.T) {
	out, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	term := Terminal{fd: -1, prev: &termState{}, out: out}
	if err := term.Close(); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("Close = %v; want terminal restore EBADF", err)
	}
}

func TestTerminalReportsInputEIO(t *testing.T) {
	master, slave := testPTY(t)
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}
	term := Terminal{fd: int(master.Fd()), in: master, keys: make(chan Key, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	go term.readKeys()
	t.Cleanup(term.stopReader)
	select {
	case _, ok := <-term.Keys():
		if ok {
			t.Fatal("input EIO emitted a key instead of closing the stream")
		}
	case <-time.After(time.Second):
		t.Fatal("input EIO did not close the key stream")
	}
	if err := term.Err(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Err after Keys closed = %v; want input EIO", err)
	}
}

func TestPasswordBackspaceRemovesWholeRune(t *testing.T) {
	for _, backspace := range []byte{0x7f, 0x08} {
		var esc escState
		input := append([]byte("é"), backspace, 'a', '\n')
		line, done, err := feedPassword(nil, input, &esc)
		if err != nil || !done || string(line) != "a" {
			t.Fatalf("Backspace %#x: password = %q, %v, %v; want a, true, nil", backspace, line, done, err)
		}
	}
}

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
