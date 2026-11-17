package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// preserveIn and preserveOut are the descriptors Preserve snapshots and checks; tests point them at a pty.
var preserveIn, preserveOut = 0, 1

// RunAttached runs argv on our stdio; Notify (not SIG_IGN, which deafens the child) eats SIGINT; true = killed.
func RunAttached(argv []string) (bool, error) {
	if len(argv) == 0 {
		return false, errors.New("proc: RunAttached: empty command")
	}
	defer Preserve()()
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...) // attached for its whole life: nothing to cancel
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	swallow := make(chan os.Signal, 1) // never read: a full buffer simply drops later signals
	signal.Notify(swallow, os.Interrupt, syscall.SIGQUIT)
	defer signal.Stop(swallow)
	err := cmd.Run()
	if ps := cmd.ProcessState; ps != nil && !ps.Exited() {
		return true, nil // a ProcessState that did not exit normally was terminated by a signal
	}
	return false, err
}
