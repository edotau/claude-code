//go:build !linux

package setup

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
)

// openMemoryFiles retains state if lsof is missing or cannot inspect open descriptors.
func scanOpenMemoryFiles() (map[string]bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "lsof", "-Fn", "+D", memstore.StateDir()).Output()
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
