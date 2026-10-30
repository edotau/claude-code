package textutil

import "fmt"

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
