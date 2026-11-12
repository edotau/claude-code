//go:build linux

package worktree

import (
	"cmp"
	"os"
	"path/filepath"
	"strconv"

	"github.com/edotau/claude-code/internal/proc"
)

func init() {
	scanProcs = scanProcsLinux
	parentOf = func(pid int) (int, error) { return proc.Parent(pid), nil } // 0 ends ancestry
	commName = func(pid int) string { return cmp.Or(proc.Comm(pid), "?") }
}

// scanProcsLinux walks /proc once; a process whose cwd cannot be read (denied or exited) is skipped.
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
