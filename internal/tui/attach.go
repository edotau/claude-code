package tui

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// RunAttached runs argv on this process's stdin/stdout/stderr. While it runs, the attached signals (SIGINT, and
// SIGQUIT on unix) are caught and dropped here, so Ctrl-C reaches only the child; afterwards they behave as before.
// Returns (false, nil) on success, (true, nil) when the child died from a signal, (false, err) otherwise.
//
// Catching (signal.Notify + signal.Stop) rather than signal.Ignore is deliberate: SIG_IGN is inherited across exec,
// so an ignoring parent would make the child deaf to Ctrl-C too, while a caught signal resets to default in the child.
// Stop removes only this channel, leaving any other Notify registration (e.g. a NotifyContext) untouched.
func RunAttached(argv []string) (bool, error) {
	if len(argv) == 0 {
		return false, errors.New("tui: RunAttached: empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	swallow := make(chan os.Signal, 1) // never read: a full buffer simply drops later signals
	signal.Notify(swallow, attachedSignals...)
	defer signal.Stop(swallow)
	err := cmd.Run()
	if ps := cmd.ProcessState; ps != nil && !ps.Exited() {
		return true, nil // a ProcessState that did not exit normally was terminated by a signal
	}
	return false, err
}
