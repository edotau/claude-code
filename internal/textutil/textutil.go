// Package textutil holds small rune-safe string-truncation and lowercase-token primitives shared across the harness.
package textutil

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Truncate cuts s to at most n runes, replacing the tail with "…" when it does not fit so the
// result (including the ellipsis) never exceeds n runes.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

// TruncateRunes hard-cuts s to the first n runes with no ellipsis, mirroring Python's s[:n] slicing.
func TruncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:max(n, 0)])
	}
	return s
}

// CapRunes keeps the first n runes of s and appends suffix when s is longer; s is returned unchanged otherwise.
func CapRunes(s string, n int, suffix string) string {
	if len(s) <= n {
		return s
	}
	if r := []rune(s); len(r) > n {
		return string(r[:max(n, 0)]) + suffix
	}
	return s
}

// RuneLen is the rune count of s.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

// Plural returns "s" when n != 1 — the terse counter suffix for log lines.
func Plural(n int) string { return PluralWord(n, "", "s") }

// PluralWord picks one or many for n — the general form for suffixes Plural's bare "s" can't spell.
func PluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// IsDigits mirrors Python str.isdigit(): non-empty and all ASCII digits.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// HumanBytes renders a byte count compactly in binary units (1 decimal past B).
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		f /= unit
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", f/unit)
}

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
