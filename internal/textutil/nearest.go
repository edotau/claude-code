package textutil

// Nearest is the one name within edit distance 2 of want (1 for want of ≤3 runes); ok is false on none or a tie.
func Nearest(want string, names []string) (string, bool) {
	limit, best, bestD, tie := 2, "", 1<<30, false
	if len([]rune(want)) <= 3 {
		limit = 1
	}
	for _, n := range names {
		d := distance(want, n)
		switch {
		case d < bestD:
			best, bestD, tie = n, d, false
		case d == bestD:
			tie = true
		}
	}
	return best, bestD <= limit && !tie && best != ""
}

func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}
