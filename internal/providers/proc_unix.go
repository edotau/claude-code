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

// capBuffer bounds what a helper's output can cost: it keeps the first max bytes (or, with tail, the last max) and
// records whether anything was dropped.
type capBuffer struct {
	buf  bytes.Buffer // named, not embedded: an embedded Buffer's ReadFrom would let io.Copy bypass the cap
	max  int
	tail bool
	over bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if c.tail {
		c.buf.Write(p)
		if extra := c.buf.Len() - c.max; extra > 0 {
			c.over = true
			kept := append([]byte(nil), c.buf.Bytes()[extra:]...)
			c.buf.Reset()
			c.buf.Write(kept)
		}
		return n, nil
	}
	room := c.max - c.buf.Len()
	if len(p) > room {
		c.over = true
		p = p[:max(room, 0)]
	}
	c.buf.Write(p)
	return n, nil
}

func (c *capBuffer) Bytes() []byte { return c.buf.Bytes() }

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
	stdout, stderr := &capBuffer{max: 1 << 20}, &capBuffer{max: 32 << 10, tail: true} // stderr keeps the final error
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
	if werr == nil && stdout.over {
		return nil, fmt.Errorf("auth command printed more than %d bytes", stdout.max)
	}
	return stdout.Bytes(), werr
}
