//go:build darwin

package worktree

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func init() {
	processesIn = scanIn
	scanProcs = scanProcsDarwin
	parentOf = parentOfDarwin
	commName = commOfDarwin
}

// scanProcsDarwin shells out to lsof once (stdlib os/exec only, no cgo) since macOS has no /proc. The -F fields
// are p(id), c(omm), n(ame); lsof exits non-zero when nothing matches -d cwd, so only an exec failure is an error.
func scanProcsDarwin() ([]procCwd, error) {
	out, err := exec.CommandContext(context.Background(), "lsof", "-a", "-d", "cwd", "-Fpcn").Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, err
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

func parentOfDarwin(pid int) (int, error) {
	out, err := exec.CommandContext(context.Background(), "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

func commOfDarwin(pid int) string {
	out, err := exec.CommandContext(context.Background(), "ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "?"
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}
