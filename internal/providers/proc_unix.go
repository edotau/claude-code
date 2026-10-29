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
)

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
	var stdout, stderr bytes.Buffer
	done := make(chan struct{}, 2)
	drain := func(dst *bytes.Buffer, r *os.File) { _, _ = io.Copy(dst, r); r.Close(); done <- struct{}{} }
	if err := cmd.Start(); err != nil {
		outW.Close()
		errW.Close()
		outR.Close()
		errR.Close()
		return nil, err
	}
	outW.Close()
	errW.Close()
	go drain(&stdout, outR)
	go drain(&stderr, errR)
	werr := cmd.Wait()
	_ = killGroup() // leftovers die now, closing the pipe ends they inherited
	<-done
	<-done
	if ee, ok := errors.AsType[*exec.ExitError](werr); ok {
		ee.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), werr
}
