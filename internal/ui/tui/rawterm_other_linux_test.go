//go:build linux

package tui

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestPreserveHelper(t *testing.T) {
	mode := os.Getenv("TUI_PRESERVE_HELPER")
	if mode == "" {
		return
	}
	restoreFn := Preserve()
	if _, err := makeRaw(0); err != nil {
		os.Exit(91)
	}
	if mode == "restore" {
		restoreFn()
	}
	os.Exit(0)
}

// openPty returns the pty master and slave, skipping when /dev/ptmx is unusable.
func openPty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Skipf("TIOCSPTLCK: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Skipf("TIOCGPTN: %v", e)
	}
	s, err := os.OpenFile("/dev/pts/"+itoa(n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open pty slave: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return m, s
}

func itoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// runPreserveHelper runs the helper with the pty slave as stdin; ttyOut also makes it stdout and returns what it wrote.
func runPreserveHelper(t *testing.T, mode string, ttyOut bool) (syscall.Termios, string) {
	t.Helper()
	master, slave := openPty(t)
	var before syscall.Termios
	if err := ioctlTermios(int(slave.Fd()), ioctlGet, &before); err != nil {
		t.Fatal(err)
	}
	if before.Lflag&syscall.ECHO == 0 || before.Lflag&syscall.ICANON == 0 {
		t.Fatalf("fresh pty lacks ECHO|ICANON: %#x", before.Lflag)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := exec.Command(exe, "-test.run=^TestPreserveHelper$")
	cmd.Env = append(os.Environ(), "TUI_PRESERVE_HELPER="+mode)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, &out, &out
	if ttyOut {
		cmd.Stdout = slave
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out.String())
	}
	var after syscall.Termios
	if err := ioctlTermios(int(slave.Fd()), ioctlGet, &after); err != nil {
		t.Fatal(err)
	}
	if !ttyOut {
		return after, out.String()
	}
	// the master read has no reliable deadline, so bound it with a goroutine
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := master.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		return after, s
	case <-time.After(2 * time.Second):
		return after, ""
	}
}

func TestPreserveRestoresTermios(t *testing.T) {
	after, out := runPreserveHelper(t, "restore", true)
	if after.Lflag&syscall.ECHO == 0 || after.Lflag&syscall.ICANON == 0 {
		t.Errorf("after restore ECHO|ICANON not set: lflag=%#x", after.Lflag)
	}
	if !strings.Contains(out, "\x1b[?25h") {
		t.Errorf("restore did not show the cursor on a terminal stdout; output %q", out)
	}
}

func TestPreserveNoCursorEscapeOffTerminalStdout(t *testing.T) {
	after, out := runPreserveHelper(t, "restore", false)
	if after.Lflag&syscall.ECHO == 0 || after.Lflag&syscall.ICANON == 0 {
		t.Errorf("after restore ECHO|ICANON not set: lflag=%#x", after.Lflag)
	}
	if strings.Contains(out, "\x1b[?25h") {
		t.Errorf("show-cursor written to a non-terminal stdout: %q", out)
	}
}

// Negative control: without restore the raw state persists, so the positive tests have teeth.
func TestPreserveWithoutRestoreStaysRaw(t *testing.T) {
	after, _ := runPreserveHelper(t, "norestore", false)
	if after.Lflag&(syscall.ECHO|syscall.ICANON) != 0 {
		t.Errorf("expected raw terminal without restore: lflag=%#x", after.Lflag)
	}
}
