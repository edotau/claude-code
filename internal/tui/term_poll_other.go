//go:build !linux

package tui

import "time"

// Only the Linux build can wait on stdin with a timeout, so elsewhere the pump keeps its historical
// blocking read: waitReadable never delays and Close has no pump to join.
const (
	pollTick = 0
	canPoll  = false
)

// Accepted platform gap: non-Linux has no poll, so this is best-effort and never blocks.
func waitReadable(int, time.Duration) bool { return true }
