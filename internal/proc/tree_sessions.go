package proc

// detachedMarker is set by Detach: TreeCancel spares only the sessions whose leader carries it.
const detachedMarker = "HARNESS_DETACHED"

// killable drops Detach-started sessions (a detached setsid leader other than root, and its members); sid -1 keeps.
func killable(kids []int, root int, sid func(int) int, marked func(int) bool) []int {
	leaders := map[int]bool{}
	for _, k := range kids {
		if k != root && sid(k) == k && marked(k) {
			leaders[k] = true
		}
	}
	var out []int
	for _, k := range kids {
		if s := sid(k); !leaders[k] && (s == -1 || !leaders[s]) {
			out = append(out, k)
		}
	}
	return out
}

// isDetached is a leader that carries the marker, or whose env cannot be read (macOS hides Apple binaries' env).
func isDetached(pid int, marked, unreadable func(int) bool) bool {
	return marked(pid) || unreadable(pid)
}
