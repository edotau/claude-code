//go:build unix

package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// outputInGroup runs cmd in a group led by a sleeping sentinel and kills the group as soon as cmd exits, before its
// pipes are drained; the unreaped sentinel keeps the group id ours, so the signal never reaches a reused group.
func outputInGroup(cmd *exec.Cmd) ([]byte, error) {
	lead, release, err := startSentinel()
	if err != nil {
		return nil, err
	}
	pg := lead.Process.Pid
	killGroup := func() error { return syscall.Kill(-pg, syscall.SIGKILL) }
	defer func() { _ = killGroup(); _ = release.Close(); _ = lead.Wait() }()
	return runInGroup(cmd, pg, killGroup)
}

// startSentinel starts the sleeping process-group leader; it blocks reading a pipe until release is closed.
func startSentinel() (lead *exec.Cmd, release *os.File, err error) {
	hold, release, err := os.Pipe() // the sentinel blocks reading hold until we close release
	if err != nil {
		return nil, nil, err
	}
	lead = exec.CommandContext(context.Background(), "sh", "-c", "read _") // only sh, which the auth command needs anyway
	lead.Stdin = hold
	lead.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := lead.Start(); err != nil {
		_ = hold.Close()
		_ = release.Close()
		return nil, nil, fmt.Errorf("auth command: cannot start its process-group sentinel: %w", err)
	}
	_ = hold.Close()
	return lead, release, nil
}

// runInGroup runs cmd in process group pg with capped stdout/stderr, killing the group once cmd exits.
func runInGroup(cmd *exec.Cmd, pg int, killGroup func() error) ([]byte, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW // *os.File: no copy goroutines, so Wait returns when the shell exits
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pg}
	cmd.Cancel = killGroup
	stdout, stderr := &capBuffer{max: 1 << 20}, &capBuffer{max: 32 << 10, tail: true} // stderr keeps the final error
	done := make(chan struct{}, 2)
	drain := func(dst io.Writer, r *os.File) { _, _ = io.Copy(dst, r); done <- struct{}{} }
	if err := cmd.Start(); err != nil {
		_ = outW.Close()
		_ = errW.Close()
		_ = outR.Close()
		_ = errR.Close()
		return nil, err
	}
	_ = outW.Close()
	_ = errW.Close()
	go drain(stdout, outR)
	go drain(stderr, errR)
	werr := cmd.Wait()
	_ = killGroup() // leftovers die now, closing the pipe ends they inherited
	awaitDrain(done, outR, errR)
	if ee, ok := errors.AsType[*exec.ExitError](werr); ok {
		ee.Stderr = stderr.Bytes()
	}
	if werr == nil && stdout.over {
		return nil, fmt.Errorf("auth command printed more than %d bytes", stdout.max)
	}
	return stdout.Bytes(), werr
}

// awaitDrain waits for both copies; past authDrainLimit it closes the read ends to unblock them.
func awaitDrain(done <-chan struct{}, outR, errR *os.File) {
	expire := time.After(authDrainLimit)
	for range 2 {
		select {
		case <-done:
		case <-expire: // an escaped child still holds a pipe: closing our read ends unblocks both copies
			_ = outR.Close()
			_ = errR.Close()
			<-done
		}
	}
	_ = outR.Close()
	_ = errR.Close()
}
