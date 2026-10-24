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

// parseStatPPID extracts field 4 (ppid) after the LAST ')' of the parenthesised comm.
func parseStatPPID(stat string) int {
	i := strings.LastIndex(stat, ") ")
	if i < 0 {
		return 0
	}
	f := strings.Fields(stat[i+2:])
	if len(f) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(f[1])
	return ppid
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
	return walkDescendants(pid, children)
}

func walkDescendants(root int, children map[int][]int) []int {
	var out []int
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				queue = append(queue, c)
			}
		}
	}
	return out
}

// Comm returns pid's executable name from /proc ("" when unreadable).
func Comm(pid int) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
