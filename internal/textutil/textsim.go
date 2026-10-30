package textutil

import (
	"strings"
	"unicode"
)

// Tokenize lowercases s and splits on any non-alphanumeric run.
func Tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// TokenSet is Tokenize's output as a set, for Jaccard overlap.
func TokenSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, t := range Tokenize(s) {
		set[t] = true
	}
	return set
}

// Jaccard is |a∩b| / |a∪b|, the near-duplicate overlap between two token sets; 0 when either is empty.
func Jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}
