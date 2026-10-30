//go:build darwin

package worktree

import (
	"bufio"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func init() {
	processesIn = processesInDarwin
	parentOf = parentOfDarwin
	commName = commOfDarwin
}

// processesInDarwin shells out to lsof (stdlib os/exec only, no cgo) since macOS has no /proc. The
// -F fields are p(id), c(omm), n(ame); lsof exits non-zero when nothing matches -d cwd, which is not
// an error here, so only an exec failure with no output is treated as one.
func processesInDarwin(dir string) ([]Proc, error) {
	out, err := exec.Command("lsof", "-a", "-d", "cwd", "-Fpcn").Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, err
		}
	}
	var procs []Proc
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
			if within(line[1:], dir) {
				procs = append(procs, Proc{PID: pid, Comm: comm})
			}
		}
	}
	return procs, scanner.Err()
}

func parentOfDarwin(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

func commOfDarwin(pid int) string {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "?"
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}
