//go:build !linux && !darwin

package tui

import "time"

// Only Linux and darwin can poll stdin with a timeout; elsewhere waitReadable never delays and Close joins no pump.
const (
	pollTick = 0
	canPoll  = false
)

// Accepted platform gap: non-Linux has no poll, so this is best-effort and never blocks.
func waitReadable(int, time.Duration) bool { return true }

// pollOnce is never called here (no poll); it exists so the tests that wrap it compile on every platform.
var pollOnce func(int, time.Duration) (bool, error)
