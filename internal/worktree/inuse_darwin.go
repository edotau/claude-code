//go:build darwin

package worktree

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

func init() {
	processesIn = scanIn
	scanProcs = scanProcsDarwin
	parentOf = func(pid int) (int, error) { return proc.Parent(pid), nil } // 0 ends ancestry
	commName = func(pid int) string { return cmp.Or(proc.Comm(pid), "?") }
}

// scanProcsDarwin shells out to lsof once (stdlib os/exec only, no cgo) since macOS has no /proc. The -F fields
// are p(id), c(omm), n(ame); -w mutes warnings, so only exit 1 with empty stderr means "nothing matched".
func scanProcsDarwin() ([]procCwd, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-w", "-a", "-d", "cwd", "-Fpcn").Output()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			return nil, err
		}
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); exitErr.ExitCode() != 1 || stderr != "" {
			return nil, fmt.Errorf("lsof -d cwd: %w (%s)", err, stderr) // fail closed: no proc list means in use
		}
	}
	return parseLsofCwd(string(out))
}
