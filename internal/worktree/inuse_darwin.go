//go:build darwin

package worktree

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

func init() {
	processesIn = scanIn
	scanProcs = scanProcsDarwin
	parentOf = func(pid int) (int, error) { return proc.Parent(pid), nil } // 0 ends ancestry
	commName = commOfDarwin
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
	var procs []procCwd
	var pid int
	var comm string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			comm = ""
		case 'c':
			comm = line[1:]
		case 'n':
			procs = append(procs, procCwd{PID: pid, Cwd: line[1:], Comm: comm})
		}
	}
	return procs, scanner.Err()
}

func commOfDarwin(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "?"
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}
