package textutil

import (
	"slices"
	"testing"
)

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"shorter than n", "abc", 10, "abc"},
		{"exact fit", "abc", 3, "abc"},
		{"one over", "abcd", 3, "ab…"},
		{"multi-byte runes", "日本語abcdef", 5, "日本語a…"},
		{"n=1 always ellipsis when truncated", "abc", 1, "…"},
		{"empty string", "", 5, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Truncate(c.s, c.n); got != c.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"shorter than n", "abc", 10, "abc"},
		{"exact fit", "abc", 3, "abc"},
		{"hard cut, no ellipsis", "abcd", 3, "abc"},
		{"multi-byte runes", "日本語abcdef", 3, "日本語"},
		{"n=0", "abc", 0, ""},
		{"empty string", "", 5, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TruncateRunes(c.s, c.n); got != c.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
			}
		})
	}
}

func TestCapRunes(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"ascii over", "abcdef", 3, "abc!"},
		{"exactly n", "abc", 3, "abc"},
		{"n+1", "abcd", 3, "abc!"},
		{"multibyte", "日本語abc", 4, "日本語a!"},
		{"emoji", "😀😀😀", 2, "😀😀!"},
		{"n=0", "abc", 0, "!"},
		{"n negative", "abc", -2, "!"},
		{"empty", "", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CapRunes(c.s, c.n, "!"); got != c.want {
				t.Errorf("CapRunes(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
			}
		})
	}
}

func TestRuneLen(t *testing.T) {
	for s, want := range map[string]int{"": 0, "abc": 3, "日本語": 3, "😀a": 2} {
		if got := RuneLen(s); got != want {
			t.Errorf("RuneLen(%q) = %d, want %d", s, got, want)
		}
	}
}

// Non-positive widths must not panic.
func TestNonPositiveWidths(t *testing.T) {
	for _, n := range []int{0, -1} {
		if got := Truncate("héllo", n); got != "" {
			t.Errorf("Truncate(_, %d) = %q, want empty", n, got)
		}
		if got := TruncateRunes("héllo", n); got != "" {
			t.Errorf("TruncateRunes(_, %d) = %q, want empty", n, got)
		}
	}
}

func TestTokenize(t *testing.T) {
	got := Tokenize("Run `make-test` NOW, café 42!")
	want := []string{"run", "make", "test", "now", "café", "42"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q want %q", got, want)
	}
	if len(Tokenize("  --- ")) != 0 {
		t.Error("punctuation-only input should yield no tokens")
	}
}

func TestPlural(t *testing.T) {
	if Plural(1) != "" || Plural(0) != "s" || Plural(2) != "s" || Plural(-1) != "s" {
		t.Errorf("Plural: 1=%q 0=%q 2=%q -1=%q", Plural(1), Plural(0), Plural(2), Plural(-1))
	}
	if got := PluralWord(1, "y", "ies"); got != "y" {
		t.Errorf("PluralWord(1, y, ies) = %q, want y", got)
	}
	if got := PluralWord(2, "y", "ies"); got != "ies" {
		t.Errorf("PluralWord(2, y, ies) = %q, want ies", got)
	}
}

func TestIsDigits(t *testing.T) {
	for s, want := range map[string]bool{"": false, "0": true, "123": true, "12a": false, "-1": false, "１": false} {
		if got := IsDigits(s); got != want {
			t.Errorf("IsDigits(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestHumanBytes: binary units (KiB spelling, base 1024), 1 decimal past B, up to PiB.
func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1024*1024 + 512*1024, "1.5 MiB"},
		{1024 * 1024 * 1024 * 1024 * 1024, "1.0 PiB"},
	} {
		if got := HumanBytes(tc.n); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
