//go:build darwin

package proc

import (
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Parent returns pid's parent pid via ps (0 when unreadable): darwin has no /proc to read.
func Parent(pid int) int {
	out, err := exec.CommandContext(context.Background(), "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	ppid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid
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
	children := make(map[int][]int)
	for line := range strings.SplitSeq(string(out), "\n") {
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

// Comm returns pid's executable name via ps ("" when unreadable); ps prints a path, so base it.
func Comm(pid int) string {
	out, err := exec.CommandContext(context.Background(), "ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return ""
	}
	return filepath.Base(name)
}
