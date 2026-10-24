// Package textutil holds small rune-safe string-truncation primitives shared across the harness.
package textutil

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
	r := []rune(s)
	if len(r) > n {
		return string(r[:max(n, 0)])
	}
	return s
}
