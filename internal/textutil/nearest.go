package textutil

import "unicode/utf8"

// Nearest is the one name within edit distance 2 of want (1 for want of ≤3 runes); ok is false on none or a tie.
func Nearest(want string, names []string) (string, bool) {
	rw := []rune(want)
	limit, best, bestD, tie := 2, "", 1<<30, false
	if len(rw) <= 3 {
		limit = 1
	}
	prev, cur := make([]int, 0, 32), make([]int, 0, 32) // fits most names, so rows rarely regrow
	for _, n := range names {
		// the rune-length gap is a lower bound on the distance, so a wider gap can never be the match
		if gap := utf8.RuneCountInString(n) - len(rw); gap > limit || -gap > limit {
			continue
		}
		var d int
		d, prev, cur = distance(rw, n, prev, cur)
		switch {
		case d < bestD:
			best, bestD, tie = n, d, false
		case d == bestD:
			tie = true
		}
	}
	return best, bestD <= limit && !tie && best != ""
}

// distance is the Levenshtein distance of ra and b, reusing prev and cur as its two row buffers.
func distance(ra []rune, b string, prev, cur []int) (int, []int, []int) {
	rb := []rune(b)
	prev, cur = prev[:0], cur[:0]
	for j := 0; j <= len(rb); j++ {
		prev, cur = append(prev, j), append(cur, 0)
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)], prev, cur
}
