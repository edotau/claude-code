//go:build windows

package clean

// pidAlive: no signal-0 probe on windows; treat every pid as alive so dead-session pruning no-ops.
var pidAlive = func(pid int) bool { return true }
