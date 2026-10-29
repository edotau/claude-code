//go:build unix

package agent_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/cli"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

// batchPipeHelper is the child process of TestBatchClosedStdoutCancelsAndReapsChildren: it runs a real batch whose
// stdout reader is gone, with one slow task whose fake CLI records its pid.
func TestBatchPipeHelper(t *testing.T) {
	pidfile := os.Getenv("BATCH_PIPE_PIDFILE")
	if pidfile == "" {
		t.Skip("helper process only")
	}
	cliEnv(t)
	pathstest.FakeVendor(t, "claude", `p=$(cat); case "$p" in *SLOW*) echo $$ > `+pidfile+`; sleep 60;; esac`+"\n"+
		"for i in $(seq 1 100); do [ -s "+pidfile+" ] && break; sleep 0.05; done\n"+
		`printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "slow", "agent": "claude", "prompt": "SLOW", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "fast", "agent": "claude", "prompt": "quick", "workdir": t.TempDir(), "tools": true}))
	os.Exit(cli.Run([]string{"claude-code", "ask", "--batch", file, "--jobs", "2"}))
}

// A closed stdout must not kill the process with SIGPIPE: the batch cancels, exits non-zero, and its children die.
func TestBatchClosedStdoutCancelsAndReapsChildren(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBatchPipeHelper$")
	cmd.Env = append(os.Environ(), "BATCH_PIPE_PIDFILE="+pidfile)
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close() // nobody reads the batch's stdout any more
	w.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the batch did not exit after its stdout closed")
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signaled() || ws.ExitStatus() == 0 {
		t.Fatalf("exit status %v, want a normal non-zero exit (SIGPIPE must not kill the process)", cmd.ProcessState)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("the slow child never started: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	for range 60 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("child %d is still running after the batch exited", pid)
}
