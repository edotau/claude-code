package proc

// killable drops detached sessions (a setsid leader other than root, and its members) from kids; sid -1 keeps.
func killable(kids []int, root int, sid func(int) int) []int {
	leaders := map[int]bool{}
	for _, k := range kids {
		if k != root && sid(k) == k {
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
