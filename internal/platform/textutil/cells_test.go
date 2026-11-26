package textutil

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
	"unsafe"
)

func TestDisplayWidth(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want int
	}{
		{"", 0}, {"hello", 5}, {"café", 4}, {"cafe\u0301", 4},
		{"日本語", 6}, {"한글", 4}, {"カナ", 4}, {"Ａ", 2}, {"𠀀", 2},
		{"#️⃣", 2}, {"x#️⃣", 3}, {"😀", 2}, {"✈️", 2}, {"👨‍👩‍👧‍👦", 2}, {"👍🏽", 2}, {"🇺🇸", 2},
		{"↑·Σ", 3}, {"㉈", 1}, {"e\u0903", 1}, {"a\t\n\r\x00\x7fb", 2}, {"\u0085\u200d\ufe0f", 0},
		{"\x1b[31mred\x1b[0m", 3}, {"\x1b[38;2;1;2;3m界\x1b[0m", 2},
		{"\x1b]0;title\aabc", 3}, {"\x1b]8;;https://example.org\x1b\\界\x1b]8;;\x1b\\", 2},
		{"a\x1b[31", 1}, {"a\x1b]title", 1}, {"a\x1b", 1},
		{"\x1b[31\x1b[0mabc", 3}, {"\x1b]0;t\x1b[0mabc", 3}, {"\x1b[31\x1b", 0},
		{"b\u200d\x1b[0mcd", 3}, {"\x1b(B\x1b[mabc", 3}, {"\x1bé", 1},
	} {
		t.Run(fmt.Sprintf("%q", tc.s), func(t *testing.T) {
			if got := DisplayWidth(tc.s); got != tc.want {
				t.Fatalf("width = %d, want %d", got, tc.want)
			}
		})
	}
	for i, span := range cellWideRanges {
		if span[0] > span[1] || i > 0 && cellWideRanges[i-1][1] >= span[0] {
			t.Fatalf("unsorted range %v", span)
		}
	}
}

func TestTruncateCells(t *testing.T) {
	for _, tc := range []struct {
		s            string
		n            int
		marker, want string
	}{
		{"abc", 3, "…", "abc"}, {"abc", 2, "…", "a…"}, {"abc", 0, "…", ""},
		{"abc", -1, "…", ""}, {"abcd", 2, "...", ".."}, {"界界", 3, "…", "界…"},
		{"界界", 2, "…", "…"}, {"éabcd", 2, "…", "é…"},
		{"👨‍👩‍👧‍👦abc", 3, "…", "👨‍👩‍👧‍👦…"}, {"✈️abc", 2, "…", "…"},
		{"\x1b[31mabcd\x1b[0m", 3, "…", "\x1b[31mab…\x1b[0m"},
		{"a\x1b[31", 1, "…", "a\x1b[31"},
		{"\x1b]8;;http://x\x1b\\abcdef\x1b]8;;\x1b\\", 3, "…", "\x1b]8;;http://x\x1b\\ab…\x1b[0m\x1b]8;;\x1b\\"},
		{"\x1b]8;;u\a\x1b[31mabcdef", 3, "…", "\x1b]8;;u\a\x1b[31mab…\x1b[0m\x1b]8;;\x1b\\"},
		{"\x1b]8;;u\x1b\\ab\x1b]8;;\x1b\\cdef", 3, "…", "\x1b]8;;u\x1b\\ab\x1b]8;;\x1b\\…\x1b[0m"},
	} {
		got := TruncateCells(tc.s, tc.n, tc.marker)
		if got != tc.want {
			t.Errorf("truncate(%q,%d,%q) = %q, want %q", tc.s, tc.n, tc.marker, got, tc.want)
		}
	}
}

func TestTruncateCellsBoundaries(t *testing.T) {
	for _, s := range []string{"a界b😀c", "x#️⃣abc", "a✈️b", "👨‍👩‍👧‍👦xyz", "éabcd", "👍🏽abcd",
		"\x1b[31mabcdef\x1b[0m", "\x1b]0;title\aabcdef", "\x1b]0;title\x1b\\abcdef",
		"\x1b]8;;http://x\x1b\\abcdef\x1b]8;;\x1b\\",
		"\x1b[31mb\u200d\x1b[0mcd", "😀\u200d\x1b[31m🇺🇸✈️", "\x1b(B\x1b[mabc"} {
		for n := 0; n <= DisplayWidth(s); n++ {
			for _, marker := range []string{"…", "...", "界", ""} {
				got := TruncateCells(s, n, marker)
				if DisplayWidth(got) > n || !utf8.ValidString(got) {
					t.Fatalf("invalid clip %q at %d: %q", s, n, got)
				}
				assertCellEscapes(t, got)
			}
		}
	}
}

func assertCellEscapes(t *testing.T, s string) {
	t.Helper()
	for i := 0; i < len(s); {
		end, _ := cellToken(s, i)
		if s[i] == 0x1b && !completeEscape(s[i:end]) {
			t.Fatalf("incomplete escape: %q", s)
		}
		i = end
	}
}

func TestTruncateCellsASCIICompatibility(t *testing.T) {
	const asciiCases = 400
	for length := range asciiCases {
		var b strings.Builder
		for i := range length {
			b.WriteByte(byte(32 + (i*17+length)%95))
		}
		s := b.String()
		for _, n := range []int{-1, 0, 1, 2, 7, 40, 80, length, length + 1} {
			if got, want := TruncateCells(s, n, "…"), Truncate(s, n); got != want {
				t.Fatalf("length %d, budget %d: %q != %q", length, n, got, want)
			}
		}
	}
}

func TestPadCells(t *testing.T) {
	for _, s := range []string{"", "abc", "界", "é", "✈️", "👨‍👩‍👧‍👦", "\x1b[31mred\x1b[0m"} {
		for n := range 12 {
			for _, left := range []bool{false, true} {
				got := PadCells(s, n, left)
				if DisplayWidth(got) != max(n, DisplayWidth(s)) {
					t.Fatalf("pad(%q,%d,%t) = %q", s, n, left, got)
				}
				if left && !strings.HasSuffix(got, s) || !left && !strings.HasPrefix(got, s) {
					t.Fatalf("wrong side: %q", got)
				}
			}
		}
	}
}

func TestCellsUnchangedAllocations(t *testing.T) {
	const allocationRuns = 100
	for _, s := range []string{strings.Repeat("a", 80), "界👨‍👩‍👧‍👦", "\x1b[31mred\x1b[0m"} {
		n := DisplayWidth(s)
		if got := TruncateCells(s, n, "…"); unsafe.StringData(got) != unsafe.StringData(s) {
			t.Fatal("clip copied")
		}
		if got := PadCells(s, n, false); unsafe.StringData(got) != unsafe.StringData(s) {
			t.Fatal("pad copied")
		}
		if allocs := testing.AllocsPerRun(allocationRuns, func() {
			cellsBenchWidth = DisplayWidth(s)
			cellsBenchText = PadCells(TruncateCells(s, n, "…"), n, false)
		}); allocs != 0 {
			t.Fatalf("unchanged allocated %v", allocs)
		}
	}
}

var cellsBenchWidth int
var cellsBenchText string

func BenchmarkCells(b *testing.B) {
	const paddedWidth = 100
	for _, tc := range []struct{ name, s string }{
		{"ASCII", strings.Repeat("a", 80)}, {"Color", "\x1b[31m" + strings.Repeat("a", 80) + "\x1b[0m"},
		{"CJK", strings.Repeat("row日本語", 10)}, {"Emoji", strings.Repeat("👨‍👩‍👧‍👦😀", 10)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.Run("Width", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					cellsBenchWidth = DisplayWidth(tc.s)
				}
			})
			b.Run("Clip", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					cellsBenchText = TruncateCells(tc.s, 40, "…")
				}
			})
			b.Run("WidthAndClip", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					cellsBenchWidth = DisplayWidth(tc.s)
					cellsBenchText = TruncateCells(tc.s, 40, "…")
				}
			})
			b.Run("Pad", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					cellsBenchText = PadCells(tc.s, paddedWidth, false)
				}
			})
		})
	}
}

func BenchmarkCellsPreviousASCII(b *testing.B) {
	s := strings.Repeat("a", 80)
	b.Run("RuneCount", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			cellsBenchWidth = utf8.RuneCountInString(s)
		}
	})
	b.Run("Clip", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			cellsBenchText = Truncate(s, 40)
		}
	})
	b.Run("WidthAndClip", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			cellsBenchWidth = utf8.RuneCountInString(s)
			cellsBenchText = Truncate(s, 40)
		}
	})
}

func ExampleDisplayWidth() {
	fmt.Println(DisplayWidth("\x1b[31m日本語\x1b[0m"))
	// Output: 6
}

func ExampleTruncateCells() {
	fmt.Println(TruncateCells("日本語のタイトル", 7, "…"))
	// Output: 日本語…
}

func TestSuffixCells(t *testing.T) {
	const allocationRuns = 100
	for _, tc := range []struct {
		name, s string
		n       int
		want    string
	}{
		{"ASCII", "abcdef", 3, "def"},
		{"CJK", "日本語", 3, "語"},
		{"wide boundary", "a界b", 2, "b"},
		{"ZWJ cluster", "x👨‍👩‍👧‍👦", 2, "👨‍👩‍👧‍👦"},
		{"cluster straddles", "x👨‍👩‍👧‍👦", 1, ""},
		{"flag cluster", "x🇺🇸", 2, "🇺🇸"},
		{"combining cluster", "xé", 1, "é"},
		{"escape boundary", "ab\x1b[31mcd", 2, "\x1b[31mcd"},
		{"earlier escape", "\x1b[31mabcd\x1b[0m", 2, "cd\x1b[0m"},
		{"OSC boundary", "ab\x1b]0;title\acd", 2, "\x1b]0;title\acd"},
		{"zero", "abc", 0, ""},
		{"negative", "abc", -1, ""},
		{"empty", "", 1, ""},
		{"fits ASCII", "abc", 3, "abc"},
		{"fits wide", "界👨‍👩‍👧‍👦", 5, "界👨‍👩‍👧‍👦"},
		{"fits color", "\x1b[31mred\x1b[0m", 3, "\x1b[31mred\x1b[0m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SuffixCells(tc.s, tc.n)
			if got != tc.want {
				t.Fatalf("suffix(%q,%d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
			if tc.s != "" && tc.want == tc.s && unsafe.StringData(got) != unsafe.StringData(tc.s) {
				t.Fatal("suffix copied")
			}
			if allocs := testing.AllocsPerRun(allocationRuns, func() {
				cellsBenchText = SuffixCells(tc.s, tc.n)
			}); allocs != 0 {
				t.Fatalf("suffix allocated %v", allocs)
			}
		})
	}
}
