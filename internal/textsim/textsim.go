// Package textsim holds the lowercase-token primitive behind memory-bank search.
package textsim

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
