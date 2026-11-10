//go:build unix

package gateway

import (
	"syscall"

	"github.com/edotau/claude-code/internal/proc"
)

var alive = proc.Alive

func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
