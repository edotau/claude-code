//go:build linux

package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func init() {
	processesIn = scanIn
	scanProcs = scanProcsLinux
	procCmdline = procCmdlineLinux
	parentOf = parentOfLinux
	commName = commOf
}

// scanProcsLinux walks /proc once; a readlink that fails (permission denied, or the process already exited)
// is skipped since a process we can't inspect isn't ours to report.
func scanProcsLinux() ([]procCwd, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	resolvedBy := map[string]string{}
	var procs []procCwd
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid directory
		}
		link, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil {
			continue
		}
		resolved, ok := resolvedBy[link]
		if !ok {
			if resolved, err = filepath.EvalSymlinks(link); err != nil {
				resolved = link
			}
			resolvedBy[link] = resolved
		}
		procs = append(procs, procCwd{PID: pid, Cwd: resolved})
	}
	return procs, nil
}

// procCmdlineLinux reads /proc/<pid>/cmdline (NUL-separated) as a space-joined command line.
func procCmdlineLinux(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(strings.TrimRight(string(b), "\x00"), "\x00", " "), nil
}

func commOf(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

// parentOfLinux reads ppid from /proc/<pid>/stat: fields after the ')' closing the comm field are
// state, ppid, ...
func parentOfLinux(pid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(string(b)[i+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	return strconv.Atoi(fields[1])
}
