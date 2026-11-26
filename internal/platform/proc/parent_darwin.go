//go:build darwin

package proc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
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

// PIDsNamed returns pids AgentName finds in names; a denied or empty process table is an error, never "none".
func PIDsNamed(names map[string]bool) ([]int, error) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("sysctl kern.proc.all: %w", err)
	}
	if len(all) == 0 {
		return nil, errors.New("sysctl kern.proc.all returned no processes")
	}
	var pids []int
	for i := range all {
		pid := int(all[i].Proc.P_pid)
		if AgentName(cString(all[i].Proc.P_comm[:]), func() string { return Exe(pid) }, names) != "" {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// cString is b up to its first NUL.
func cString(b []byte) string {
	n, _, _ := strings.Cut(string(b), "\x00")
	return n
}

// Descendants returns pid's children, grandchildren, ... from the kernel process table.
func Descendants(pid int) []int {
	if pid <= 0 {
		return nil
	}
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	children := make(map[int][]int)
	for i := range all {
		cpid, ppid := int(all[i].Proc.P_pid), int(all[i].Eproc.Ppid)
		if cpid > 1 && ppid > 0 {
			children[ppid] = append(children[ppid], cpid)
		}
	}
	for _, kids := range children {
		slices.Sort(kids)
	}
	return walkChildren(children, pid)
}

// Comm returns pid's executable name ("" when unreadable): argv[0]'s base as ps prints it, else asked of ps.
func Comm(pid int) string {
	if name := argv0(pid); name != "" {
		return filepath.Base(name)
	}
	return psComm(pid)
}

// Exe is pid's exec path as launched (a symlink stays unresolved) from procargs2; "" when unreadable.
func Exe(pid int) string {
	exe, _ := procArgs(pid)
	return exe
}

// argv0 is pid's first argument from procargs2; "" when unreadable or empty.
func argv0(pid int) string {
	_, rest := procArgs(pid)
	first, _, _ := strings.Cut(strings.TrimLeft(rest, "\x00"), "\x00")
	return strings.TrimSpace(first)
}

// procArgs splits procargs2 (argc, exec path, NUL padding, argv) into the exec path and the rest; argc 0 is unreadable.
func procArgs(pid int) (exe, rest string) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) <= 4 || binary.NativeEndian.Uint32(raw) == 0 {
		return "", ""
	}
	exe, rest, _ = strings.Cut(string(raw[4:]), "\x00")
	return exe, rest
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
