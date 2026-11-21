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

// Truncate cuts s to at most n runes, ending in "…" when it does not fit, so the result never exceeds n.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	j, more := runeCut(s, n-1)
	if !more {
		return s
	}
	if _, size := utf8.DecodeRuneInString(s[j:]); j+size >= len(s) {
		return s
	}
	return validPrefix(s[:j]) + "…"
}

// runeCut is the byte offset after the first n runes of s and whether s has more than n runes.
func runeCut(s string, n int) (int, bool) {
	if n < 0 {
		return 0, true
	}
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i, i < len(s)
}

// validPrefix is p, with each invalid byte replaced by U+FFFD as a []rune conversion does.
func validPrefix(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	return string([]rune(p))
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
	i, more := runeCut(s, n)
	if !more {
		return s
	}
	return validPrefix(s[:i]) + suffix
}

// RuneLen is the rune count of s.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }

// OneLine renders s on one trimmed line: newlines/tabs become a space, runs of spaces collapse, other controls drop.
func OneLine(s string) string {
	s = strings.TrimSpace(s)
	if !strings.ContainsFunc(s, isControl) && !strings.Contains(s, "  ") {
		return s
	}
	out := make([]rune, 0, len(s))
	prevSpace := false
	for _, c := range s {
		if c == '\n' || c == '\r' || c == '\t' {
			c = ' '
		}
		if c == ' ' && prevSpace || isControl(c) {
			continue
		}
		prevSpace = c == ' '
		out = append(out, c)
	}
	return string(out)
}

func isControl(c rune) bool { return unicode.IsControl(c) }

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
