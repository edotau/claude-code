package helptext

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func lead(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

func TestWrapRow(t *testing.T) {
	const lw, w = 18, 80
	long := strings.Repeat("alpha beta gamma ", 12)
	tests := []struct {
		name, desc string
		wantLines  int // 0 = just "more than one"
	}{
		{"short", "one short description", 1},
		{"empty", "", 1},
		{"long", long, 0},
		{"overlong word", "see " + strings.Repeat("x", 90) + " now", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WrapRow("  name", tc.desc, lw, w)
			switch {
			case tc.wantLines > 0 && len(got) != tc.wantLines:
				t.Fatalf("lines = %d, want %d: %q", len(got), tc.wantLines, got)
			case tc.wantLines == 0 && len(got) < 2:
				t.Fatalf("want a wrap, got %q", got)
			}
			if !strings.HasPrefix(got[0], "  name"+strings.Repeat(" ", lw-6)) {
				t.Errorf("first line label not padded to %d: %q", lw, got[0])
			}
			var words []string
			for i, l := range got {
				if i > 0 && lead(l) != lw {
					t.Errorf("continuation %d starts at col %d, want %d: %q", i, lead(l), lw, l)
				}
				if utf8.RuneCountInString(l) > w && len(strings.Fields(l[lw:])) > 1 {
					t.Errorf("line %d is %d cols with several words: %q", i, utf8.RuneCountInString(l), l)
				}
				words = append(words, strings.Fields(l[min(lw, len(l)):])...)
			}
			if strings.Join(words, " ") != strings.Join(strings.Fields(tc.desc), " ") {
				t.Errorf("words changed or split: %q", words)
			}
		})
	}
	if got := WrapRow("  name", "one short description", lw, w); got[0] != "  name"+strings.Repeat(" ", 12)+"one short description" {
		t.Errorf("short row changed: %q", got[0])
	}
}

func TestPaintKeepsWrappedLayout(t *testing.T) {
	text := strings.Join(WrapRow("  name", strings.Repeat("alpha beta ", 12), 18, 80), "\n") + "\n"
	if got := ansiRE.ReplaceAllString(Paint(text), ""); got != text {
		t.Errorf("plain text of Paint differs:\n%q\nwant\n%q", got, text)
	}
}
