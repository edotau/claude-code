//go:build !linux

package clean

import (
	"errors"
	"os/exec"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
)

// openMemoryFiles retains state if lsof is missing or cannot inspect open descriptors.
func openMemoryFiles() (map[string]bool, bool) {
	data, err := exec.Command("lsof", "-Fn", "+D", memory.StateDir()).Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(exit.Stderr) != 0 {
			return nil, false
		}
	}
	opened := map[string]bool{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if p, ok := strings.CutPrefix(line, "n"); ok {
			opened[p] = true
		}
	}
	return opened, true
}
