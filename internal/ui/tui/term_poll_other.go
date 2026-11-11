//go:build !linux && !darwin

package tui

import "time"

// Only the Linux and darwin builds can wait on stdin with a timeout, so elsewhere the pump keeps its historical
// blocking read: waitReadable never delays and Close has no pump to join.
const (
	pollTick = 0
	canPoll  = false
)

// Accepted platform gap: non-Linux has no poll, so this is best-effort and never blocks.
func waitReadable(int, time.Duration) bool { return true }

// pollOnce is never called here (no poll); it exists so the tests that wrap it compile on every platform.
var pollOnce func(int, time.Duration) (bool, error)
