//go:build !windows

package bench

import (
	"os/exec"

	"github.com/edotau/claude-code/internal/proc"
)

// ownGroup runs cmd in its own process group and kills the group on cancel.
func ownGroup(cmd *exec.Cmd) { proc.OwnGroup(cmd) }
