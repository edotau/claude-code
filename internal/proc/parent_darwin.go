//go:build darwin

package proc

import (
	"context"
	"encoding/binary"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Parent returns pid's parent pid from the kernel process table (0 when unreadable): darwin has no /proc.
func Parent(pid int) int {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0
	}
	return int(kp.Eproc.Ppid)
}

// PIDsNamed returns the pids whose kernel process name (p_comm, what lsof prints as c) is in names.
func PIDsNamed(names map[string]bool) []int {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	var pids []int
	for i := range all {
		if names[cString(all[i].Proc.P_comm[:])] {
			pids = append(pids, int(all[i].Proc.P_pid))
		}
	}
	return pids
}

// cString is b up to its first NUL.
func cString(b []byte) string {
	n, _, _ := strings.Cut(string(b), "\x00")
	return n
}

// Descendants returns pid's children, grandchildren, ... via ps -A -o pid=,ppid=.
func Descendants(pid int) []int {
	if pid <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	return walkChildren(parseChildren(string(out)), pid)
}

// Comm returns pid's executable name ("" when unreadable): argv[0]'s base as ps prints it, else asked of ps.
func Comm(pid int) string {
	if name := argv0(pid); name != "" {
		return filepath.Base(name)
	}
	return psComm(pid)
}

// argv0 is pid's first argument from procargs2 (argc, exec path, NUL padding, argv); "" when unreadable or empty.
func argv0(pid int) string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) <= 4 || binary.NativeEndian.Uint32(raw) == 0 {
		return ""
	}
	_, rest, _ := strings.Cut(string(raw[4:]), "\x00")
	first, _, _ := strings.Cut(strings.TrimLeft(rest, "\x00"), "\x00")
	return strings.TrimSpace(first)
}

// psComm is ps's name for pid; it covers processes procargs2 refuses (other users', the kernel's).
func psComm(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return ""
	}
	return filepath.Base(name)
}
