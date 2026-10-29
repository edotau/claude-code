//go:build unix

package providers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// authDrainLimit bounds draining after the group is killed; a child that escaped the group (setsid) can't hang us.
var authDrainLimit = 5 * time.Second

// capBuffer keeps the first max bytes written and discards the rest, so a chatty helper can't exhaust memory.
type capBuffer struct {
	bytes.Buffer
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// outputInGroup runs cmd in a group led by a sleeping sentinel and kills the group as soon as cmd exits, before its
// pipes are drained; the unreaped sentinel keeps the group id ours, so the signal never reaches a reused group.
func outputInGroup(cmd *exec.Cmd) ([]byte, error) {
	lead := exec.Command("sleep", "86400")
	lead.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := lead.Start(); err != nil {
		return nil, fmt.Errorf("auth command: cannot start its process-group sentinel: %w", err)
	}
	pg := lead.Process.Pid
	killGroup := func() error { return syscall.Kill(-pg, syscall.SIGKILL) }
	defer func() { _ = killGroup(); _ = lead.Wait() }()
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW // *os.File: no copy goroutines, so Wait returns when the shell exits
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pg}
	cmd.Cancel = killGroup
	stdout, stderr := &capBuffer{max: 1 << 20}, &capBuffer{max: 32 << 10}
	done := make(chan struct{}, 2)
	drain := func(dst io.Writer, r *os.File) { _, _ = io.Copy(dst, r); done <- struct{}{} }
	if err := cmd.Start(); err != nil {
		outW.Close()
		errW.Close()
		outR.Close()
		errR.Close()
		return nil, err
	}
	outW.Close()
	errW.Close()
	go drain(stdout, outR)
	go drain(stderr, errR)
	werr := cmd.Wait()
	_ = killGroup() // leftovers die now, closing the pipe ends they inherited
	expire := time.After(authDrainLimit)
	for range 2 {
		select {
		case <-done:
		case <-expire: // an escaped child still holds a pipe: closing our read ends unblocks both copies
			outR.Close()
			errR.Close()
			<-done
		}
	}
	outR.Close()
	errR.Close()
	if ee, ok := errors.AsType[*exec.ExitError](werr); ok {
		ee.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), werr
}
