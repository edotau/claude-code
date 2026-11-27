//go:build linux || darwin

package statusline

import (
	"os"
	"time"

	"github.com/edotau/claude-code/internal/platform/proc"
)

// psBudget caps the whole ancestor walk.
const psBudget = time.Second

// ancestorTTYWidth is the column count of the first ancestor process attached to a terminal (0 if none).
func ancestorTTYWidth() int { return walkAncestors(os.Getppid(), psBudget, psParentTTY, ttyCols) }

func ttyCols(dev string) int {
	f, err := os.Open(dev)
	if err != nil {
		return 0
	}
	defer f.Close()
	cols, _, err := proc.Size(int(f.Fd()))
	if err != nil {
		return 0
	}
	return cols
}
