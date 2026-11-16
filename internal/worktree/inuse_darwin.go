//go:build darwin

package worktree

import (
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

func init() {
	scanProcs = scanProcsDarwin
	scanAgentProcs = scanAgentsDarwin
	parentOf = func(pid int) (int, error) { return proc.Parent(pid), nil }
	commName = func(pid int) string { return cmp.Or(proc.Comm(pid), "?") }
}

// scanProcsDarwin runs lsof once (no /proc on macOS); -w mutes warnings: only exit 1 + empty stderr = no match.
func scanProcsDarwin() ([]procCwd, error) { return lsofCwd() }

// maxAgentPIDs caps the -p list; a longer one is not an agent list, so the caller falls back to the full scan.
const maxAgentPIDs = 1000

// scanAgentsDarwin is scanProcsDarwin on agent-named pids only; zero pids is an error (this process is an agent).
func scanAgentsDarwin() ([]procCwd, error) {
	pids, err := proc.PIDsNamed(agentComms)
	if err != nil {
		return nil, err
	}
	if len(pids) == 0 || len(pids) > maxAgentPIDs {
		return nil, fmt.Errorf("agent pid list unusable (%d pids)", len(pids))
	}
	strs := make([]string, len(pids))
	for i, p := range pids {
		strs[i] = strconv.Itoa(p)
	}
	return lsofCwd("-p", strings.Join(strs, ","))
}

func lsofCwd(extra ...string) ([]procCwd, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", append([]string{"-w", "-a", "-d", "cwd", "-Fpcn"}, extra...)...).Output()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			return nil, err
		}
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); exitErr.ExitCode() != 1 || stderr != "" {
			// fail closed: no proc list means in use
			return nil, fmt.Errorf("lsof -d cwd: %w (%s)", err, stderr)
		}
	}
	return parseLsofCwd(string(out))
}
