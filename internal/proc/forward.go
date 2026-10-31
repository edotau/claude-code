package proc

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

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
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			if s == syscall.SIGTERM {
				_ = cmd.Process.Signal(s)
			}
		}
	}()
	err := cmd.Wait()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), nil
	}
	return 0, err
}
