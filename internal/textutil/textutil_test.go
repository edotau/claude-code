package textutil

import "testing"

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
