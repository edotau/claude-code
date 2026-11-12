// Package textutil holds small string, JSONC, front-matter, credential and HTTP helpers shared across the harness.
package textutil

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Title upper-cases the first rune of s ("plus" → "Plus").
func Title(s string) string {
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

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

// CutBytes is the longest prefix of s of at most n bytes that ends on a rune boundary.
func CutBytes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:max(n, 0)]
}

// TruncateRunes hard-cuts s to the first n runes with no ellipsis, mirroring Python's s[:n] slicing.
func TruncateRunes(s string, n int) string { return CapRunes(s, n, "") }

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

// PluralWord picks one or many for n.
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
		if f < unit-0.05 {
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

// SplitCSV splits a comma-separated list, trimming blanks and dropping empty entries; nil when none remain.
func SplitCSV(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// SHA256Hex is the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
