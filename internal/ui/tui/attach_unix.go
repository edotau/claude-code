//go:build linux || darwin

package tui

import (
	"os"
	"syscall"
)

// attachedSignals are the terminal-generated signals RunAttached keeps away from this process.
var attachedSignals = []os.Signal{os.Interrupt, syscall.SIGQUIT}
