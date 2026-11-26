package rtkwrap

import (
	"os"
	"strconv"
	"strings"
)

// argvOf is pid's argv from /proc/<pid>/cmdline; nil when unreadable.
func argvOf(pid int) []string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(raw) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}
