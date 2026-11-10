package proc

import (
	"strconv"
	"strings"
)

// parseStatPPID extracts field 4 (ppid) after the LAST ')' of the parenthesised comm.
func parseStatPPID(stat string) int {
	_, after, ok := strings.CutLast(stat, ") ")
	if !ok {
		return 0
	}
	f := strings.Fields(after)
	if len(f) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(f[1])
	return ppid
}

// parseChildren maps each parent pid to its child pids from `ps -A -o pid=,ppid=` output.
func parseChildren(out string) map[int][]int {
	children := make(map[int][]int)
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		cpid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil && cpid > 1 && ppid > 0 {
			children[ppid] = append(children[ppid], cpid)
		}
	}
	return children
}

// walkChildren returns every descendant of pid, breadth first.
func walkChildren(children map[int][]int, pid int) []int {
	var res []int
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if !seen[c] {
				seen[c] = true
				res = append(res, c)
				queue = append(queue, c)
			}
		}
	}
	return res
}
