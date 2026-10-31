// Package textutil holds small rune-safe string-truncation and lowercase-token primitives shared across the harness.
package textutil

import "unicode/utf8"

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
