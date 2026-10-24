package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPreview(t *testing.T) {
	cases := []struct {
		name, in string
		n        int
		want     string
	}{
		{"collapses whitespace", "a  b\n\tc   d", 100, "a b c d"},
		{"redacts credential word", "use sk-abcdef123456 now", 100, "use REDACTED now"},
		{"short input unchanged", "hello world", 11, "hello world"},
		{"n above length unchanged", "hello", 50, "hello"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Preview(c.in, c.n); got != c.want {
				t.Errorf("Preview(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

func TestPreviewTruncatesOnRuneBoundary(t *testing.T) {
	// Given a prompt of 4-byte runes longer than n; When truncated; Then the result is valid UTF-8 and ends in "…"
	got := Preview(strings.Repeat("😀", 30), 10)
	if !utf8.ValidString(got) {
		t.Fatalf("Preview split a rune: %q", got)
	}
	if !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) != 10 {
		t.Errorf("Preview = %q (%d runes), want 10 runes ending in …", got, utf8.RuneCountInString(got))
	}
}

func TestPreviewRedactsThenTruncates(t *testing.T) {
	tok := "ghp_" + strings.Repeat("a", 36)
	got := Preview("tok "+tok+" tail words here", 12)
	if want := "tok REDACTE…"; got != want { // redacted first (no ghp_), then cut to 12 runes
		t.Errorf("Preview = %q, want %q", got, want)
	}
}
