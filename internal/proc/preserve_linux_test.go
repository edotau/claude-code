package proc

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestPreserveHelper(t *testing.T) {
	mode := os.Getenv("PROC_PRESERVE_HELPER")
	if mode == "" {
		return
	}
	restoreFn := Preserve()
	if err := rawStdin(); err != nil {
		os.Exit(91)
	}
	if mode == "restore" {
		restoreFn()
	}
	os.Exit(0)
}

// runPreserveHelper runs the helper with the pty slave as stdin; ttyOut also makes it stdout and returns what it wrote.
func runPreserveHelper(t *testing.T, mode string, ttyOut bool) (syscall.Termios, string) {
	t.Helper()
	master, slave := hosttest.PTY(t)
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
	cmd.Env = append(os.Environ(), "PROC_PRESERVE_HELPER="+mode)
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
	return after, readMaster(master)
}

// readMaster returns what one master read yields within 2s; the read has no deadline, so a goroutine bounds it.
func readMaster(master *os.File) string {
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := master.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(2 * time.Second):
		return ""
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

// rawStdin clears ECHO and ICANON on stdin, as a raw-mode prompt would.
func rawStdin() error {
	var tm syscall.Termios
	if err := ioctlTermios(0, ioctlGet, &tm); err != nil {
		return err
	}
	tm.Lflag &^= syscall.ECHO | syscall.ICANON
	return ioctlTermios(0, ioctlSet, &tm)
}
