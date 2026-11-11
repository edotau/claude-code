// Package proc owns process replacement (launch handoff) and detached spawns.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ErrNoProcess means the pid does not exist; any other StartTime error is a failed probe.
var ErrNoProcess = errors.New("proc: no such process")

// SpawnDetached starts bin+args in its own session, output on out (nil = discarded), reaped in a goroutine.
// Background(): the spawned process outlives this call by design, so there is nothing to cancel.
func SpawnDetached(bin string, args []string, dir string, out *os.File, extraEnv ...string) bool {
	if IsSelfUnderTest(bin) {
		return false
	}
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if out != nil { // a typed-nil *os.File would defeat exec's /dev/null default
		cmd.Stdout, cmd.Stderr = out, out
	}
	Detach(cmd)
	if cmd.Start() != nil {
		return false
	}
	go func() { _ = cmd.Wait() }() // reap so a long-lived parent never keeps a zombie
	return true
}

// errExecUnderTest: under `go test`, Exec would replace the test binary and end the run with the child's status.
var errExecUnderTest = errors.New("proc: refusing to replace the go test binary; stub the exec seam")

// Exec hands this process to bin (execve on unix, child + exit elsewhere); returns only on failure.
func Exec(bin string, argv, env []string) error {
	if testing.Testing() {
		return fmt.Errorf("%w: %s", errExecUnderTest, bin)
	}
	return execImage(bin, argv, env)
}

// IsSelfUnderTest: under `go test`, os.Executable is the test binary; relaunching it re-runs every test (fork bomb).
func IsSelfUnderTest(bin string) bool {
	if !testing.Testing() {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return true
	}
	a, _ := filepath.EvalSymlinks(self)
	b, _ := filepath.EvalSymlinks(bin)
	return bin == self || (a != "" && a == b)
}

// RunForwarding runs bin and returns its exit code, -1 if a signal killed it (err only if it could not start or wait).
func RunForwarding(bin string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(context.Background(), bin, argv...)
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// The tty already delivers SIGINT to the child; the parent only survives it and forwards SIGTERM.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	defer signal.Stop(sigs)
	defer close(done)
	go func() {
		for {
			select {
			case s := <-sigs:
				if s == syscall.SIGTERM {
					_ = cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// Argv returns pid's argv via /proc on Linux (NUL-split), falling back to `ps -o command=` split on spaces.
func Argv(pid int) ([]string, error) {
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// MaxCaptureBytes bounds what OutputCapped and CombinedOutputCapped keep of a command's output.
const MaxCaptureBytes = 1 << 20

// ExecWaitDelay force-closes a timed-out command's pipes so surviving children cannot hold Wait open.
const ExecWaitDelay = 2 * time.Second

// capBuffer collects writes up to max, then fails the write so the child is cut off with memory bounded.
type capBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *capBuffer) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		w.over = true
		return 0, fmt.Errorf("output over %d bytes", w.max)
	}
	return w.buf.Write(p)
}

// OutputCapped is cmd.Output() with stdout held to MaxCaptureBytes: over it, the command fails with an over-cap error.
func OutputCapped(cmd *exec.Cmd) ([]byte, error) {
	stdout := &capBuffer{max: MaxCaptureBytes}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err := cmd.Run()
	if stdout.over { // the child's SIGPIPE exit would otherwise mask the cap
		return stdout.buf.Bytes(), fmt.Errorf("output over %d bytes", stdout.max)
	}
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && ee.Stderr == nil {
		ee.Stderr = stderr.Bytes()
	}
	return stdout.buf.Bytes(), err
}

// CombinedOutputCapped is cmd.CombinedOutput() with the output held to MaxCaptureBytes, like OutputCapped.
func CombinedOutputCapped(cmd *exec.Cmd) ([]byte, error) {
	out := &capBuffer{max: MaxCaptureBytes}
	cmd.Stdout, cmd.Stderr = out, out // one writer, so exec feeds both streams through a single copier
	err := cmd.Run()
	if out.over {
		return out.buf.Bytes(), fmt.Errorf("output over %d bytes", out.max)
	}
	return out.buf.Bytes(), err
}
