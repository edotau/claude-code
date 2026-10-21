//go:build !linux && !darwin

package worktree

// No live-process detector on this OS; processesIn stays nil and InUseBy skips that check.
func init() {}
