package proc

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

// TestIsTerminalRejectsDevNullAndFiles: /dev/null is a char device but never a terminal.
func TestIsTerminalRejectsDevNullAndFiles(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if IsTerminal(null) {
		t.Error("IsTerminal(/dev/null) = true, want false")
	}
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Error("IsTerminal(regular file) = true, want false")
	}
}

func TestIsTerminalRejectsDevZero(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("termios terminal detection is supported on Linux and Darwin")
	}
	f, err := os.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	if IsTerminal(f) {
		t.Error("IsTerminal(/dev/zero) = true, want false")
	}
}

// sttyOn runs stty on the terminal f and returns its output split into words.
func sttyOn(t *testing.T, f *os.File, args ...string) []string {
	t.Helper()
	stty := "/bin/stty" // hosttest's PATH allowlist leaves stty out; macOS ships it here, Linux here or in /usr/bin
	if _, err := os.Stat(stty); err != nil {
		stty = "/usr/bin/stty"
	}
	if _, err := os.Stat(stty); err != nil {
		t.Skipf("no stty: %v", err)
	}
	cmd := exec.Command(stty, args...)
	cmd.Stdin = f
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stty %v: %v: %s", args, err, out)
	}
	return strings.Fields(string(out))
}

// cooked reports whether both ECHO and ICANON are set in an `stty -a` listing.
func cooked(flags []string) bool {
	return slices.Contains(flags, "echo") && slices.Contains(flags, "icanon")
}

// preserveOnPTY runs Preserve with a pty as stdin, makes it raw, optionally restores; returns the flags and stdout.
func preserveOnPTY(t *testing.T, restore, ttyOut bool) (flags []string, out string) {
	t.Helper()
	master, slave := hosttest.PTY(t)
	if before := sttyOn(t, slave, "-a"); !cooked(before) {
		t.Fatalf("fresh pty lacks echo|icanon: %v", before)
	}
	stdout := slave
	if !ttyOut {
		f, err := os.CreateTemp(t.TempDir(), "stdout")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		stdout = f
	}
	oldIn, oldOut, oldStdout := preserveIn, preserveOut, os.Stdout
	preserveIn, preserveOut, os.Stdout = int(slave.Fd()), int(stdout.Fd()), stdout
	t.Cleanup(func() { preserveIn, preserveOut, os.Stdout = oldIn, oldOut, oldStdout })
	restoreFn := Preserve()
	sttyOn(t, slave, "-echo", "-icanon") // as a raw-mode prompt would
	if restore {
		restoreFn()
	}
	flags = sttyOn(t, slave, "-a")
	if ttyOut {
		return flags, readMaster(master)
	}
	b, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	return flags, string(b)
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
	flags, out := preserveOnPTY(t, true, true)
	if !cooked(flags) {
		t.Errorf("after restore echo|icanon not set: %v", flags)
	}
	if !strings.Contains(out, "\x1b[?25h") {
		t.Errorf("restore did not show the cursor on a terminal stdout; output %q", out)
	}
}

func TestPreserveNoCursorEscapeOffTerminalStdout(t *testing.T) {
	flags, out := preserveOnPTY(t, true, false)
	if !cooked(flags) {
		t.Errorf("after restore echo|icanon not set: %v", flags)
	}
	if strings.Contains(out, "\x1b[?25h") {
		t.Errorf("show-cursor written to a non-terminal stdout: %q", out)
	}
}

// Negative control: without restore the raw state persists, so the positive tests have teeth.
func TestPreserveWithoutRestoreStaysRaw(t *testing.T) {
	flags, _ := preserveOnPTY(t, false, false)
	if slices.Contains(flags, "echo") || slices.Contains(flags, "icanon") {
		t.Errorf("expected raw terminal without restore: %v", flags)
	}
}
