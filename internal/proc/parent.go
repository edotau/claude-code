package proc

import (
	"path/filepath"
	"strconv"
	"strings"
)

// AgentName is comm if named, else for a version-named binary its exec base or the <agent> of <agent>/versions/<v>.
func AgentName(comm string, exe func() string, names map[string]bool) string {
	if names[comm] {
		return comm
	}
	if !versionLike(comm) {
		return ""
	}
	path := exe()
	base := filepath.Base(path)
	if names[base] {
		return base
	}
	dir := filepath.Dir(path)
	if agent := filepath.Base(filepath.Dir(dir)); versionLike(base) && filepath.Base(dir) == "versions" && names[agent] {
		return agent
	}
	return ""
}

// versionLike reports a release-number name such as "2.1.325" or "1.0.0-beta.2": a digit first, then a dot somewhere.
func versionLike(s string) bool {
	if s == "" || s[0] < '0' || s[0] > '9' || !strings.Contains(s, ".") {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return !strings.ContainsRune(versionChars, r) })
}

const versionChars = "0123456789.-+abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

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
