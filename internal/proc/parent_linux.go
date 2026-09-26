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
	// Fields after the parenthesised comm (which may itself contain spaces): state ppid ...
	_, rest, ok := strings.Cut(string(raw), ") ")
	if !ok {
		return 0
	}
	f := strings.Fields(rest)
	if len(f) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(f[1])
	return ppid
}

// Comm returns pid's executable name from /proc ("" when unreadable).
func Comm(pid int) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
