package hooks

import (
	"os"

	"github.com/edotau/claude-code/internal/proc"
)

// sessionOwnerPid is the Claude Code process behind this hook: settings.json runs hooks via `sh -c`, so skip shells.
func sessionOwnerPid() int {
	pid := os.Getppid()
	for range 8 {
		switch proc.Comm(pid) {
		case "sh", "dash", "bash", "zsh":
			if pp := proc.Parent(pid); pp > 0 {
				pid = pp
				continue
			}
			return 0 // a shell whose parent is unreadable: fail open (0 is never "dead")
		}
		return pid
	}
	return pid
}
