package proc

import (
	"os"
	"strconv"
	"strings"
)

// Parent returns pid's parent pid from /proc (0 when unreadable).
func Parent(pid int) int {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	return parseStatPPID(string(raw))
}

// Descendants returns pid's children, grandchildren, ... via /proc/*/stat.
func Descendants(pid int) []int {
	if pid <= 0 {
		return nil
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	children := make(map[int][]int)
	for _, e := range ents {
		cpid, err := strconv.Atoi(e.Name())
		if err != nil || cpid <= 1 {
			continue
		}
		if ppid := Parent(cpid); ppid > 0 {
			children[ppid] = append(children[ppid], cpid)
		}
	}
	return walkChildren(children, pid)
}

// Comm returns pid's executable name from /proc ("" when unreadable).
func Comm(pid int) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
