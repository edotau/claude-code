//go:build !linux && !darwin

package tui

import "os"

// attachedSignals: off unix only the portable interrupt exists.
var attachedSignals = []os.Signal{os.Interrupt}
