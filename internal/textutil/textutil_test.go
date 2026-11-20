package textutil

import (
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestCompareVersionsDottedPrerelease(t *testing.T) {
	if cmp, ok := CompareVersions("1.2.3-rc.1", "1.2.3"); cmp != 0 || !ok {
		t.Fatalf("CompareVersions(dotted prerelease, release) = (%d, %v), want (0, true)", cmp, ok)
	}
}

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
		{"invalid byte in prefix", "a\xffbcdef", 4, "a\uFFFDb…"},
		{"invalid byte past cut", "abcdef\xff", 4, "abc…"},
		{"rune at the cut", "ab日本語", 4, "ab日…"},
		{"n negative", "abc", -1, ""},
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
		{"invalid byte in prefix", "a\xffbcd", 3, "a\uFFFDb!"},
		{"invalid bytes within n", "a\xff", 3, "a\xff"},
		{"rune at the cut", "日本語abc", 3, "日本語!"},
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

func TestOneLine(t *testing.T) {
	if got := OneLine("ok\x1b[2J line one\nline\t two"); got != "ok[2J line one line two" {
		t.Errorf("OneLine = %q, want %q", got, "ok[2J line one line two")
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

func TestPluralWord(t *testing.T) {
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
		{1024*1024 - 1, "1.0 MiB"},
		{1024*1024 + 512*1024, "1.5 MiB"},
		{1024 * 1024 * 1024 * 1024 * 1024, "1.0 PiB"},
	} {
		if got := HumanBytes(tc.n); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestParseRetryAfterHugeSecondsNeverNegative(t *testing.T) {
	for _, v := range []string{"10000000000", "9223372036854775807"} {
		d, ok := ParseRetryAfter(v, time.Now())
		if !ok || d <= 0 {
			t.Errorf("ParseRetryAfter(%q) = %v, %v; want positive, true", v, d, ok)
		}
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

func TestSHA256Hex(t *testing.T) {
	for in, want := range map[string]string{
		"":    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"abc": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
	} {
		if got := SHA256Hex([]byte(in)); got != want {
			t.Errorf("SHA256Hex(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestScanRawLines(t *testing.T) {
	cases := []struct {
		name, in     string
		completeOnly bool
		want         []string
		wantN        int64
	}{
		{"keeps terminators and tail", "a\r\nb\nc", false, []string{"a\r\n", "b\n", "c"}, 6},
		{"complete only drops tail", "a\nb\nc", true, []string{"a\n", "b\n"}, 4},
		{"empty", "", true, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			n, err := ScanRawLines(strings.NewReader(c.in), c.completeOnly, func(l []byte) { got = append(got, string(l)) })
			if err != nil || n != c.wantN || !slices.Equal(got, c.want) {
				t.Errorf("got %q, %d, %v; want %q, %d", got, n, err, c.want, c.wantN)
			}
		})
	}
}

func TestIsNonPublicIPCGNATFlag(t *testing.T) {
	cgnat := net.ParseIP("100.64.0.1")
	if IsNonPublicIP(cgnat, false) || !IsNonPublicIP(cgnat, true) {
		t.Errorf("CGNAT must follow blockCGNAT: false=%v true=%v", IsNonPublicIP(cgnat, false), IsNonPublicIP(cgnat, true))
	}
	for _, s := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "ff02::1", "0.1.2.3", "240.0.0.1", "::127.0.0.1", "::ffff:10.0.0.1",
		"192.0.0.1", "192.0.2.1", "198.19.0.1", "198.51.100.1", "203.0.113.1", "64:ff9b::a00:1", "2001::1"} {
		if !IsNonPublicIP(net.ParseIP(s), false) {
			t.Errorf("IsNonPublicIP(%s) = false, want true", s)
		}
	}
	if !IsNonPublicIP(nil, false) || IsNonPublicIP(net.ParseIP("8.8.8.8"), true) {
		t.Error("nil is non-public and 8.8.8.8 public")
	}
}

// Headings finds the headings a CommonMark parse finds: fences, comments and indented code hide "#"; setext counts.
func TestHeadings(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"atx levels and closing hashes", "# a\n## b ##\n### c #x\n#### d\\#\n", "1:a|2:b|3:c #x|4:d\\#"},
		{"not headings", "#x\n####### seven\n    ## code\n\\## esc\n", ""},
		{"empty atx", "##\n## \n### ###\n", "2:|2:|3:"},
		{"backtick fence", "```go\n## in\n```\n## out\n", "2:out"},
		{"tilde fence longer than three", "~~~~\n## in\n~~~\n## still in\n~~~~\n## out\n", "2:out"},
		{"fence inside fence needs matching char", "```\n~~~\n## in\n```\n## out\n", "2:out"},
		{"indented fence", "   ```\n## in\n   ```\n## out\n", "2:out"},
		{"unclosed fence", "```\n## in\n", ""},
		{"setext", "Title\n=====\nSub\n---\n", "1:Title|2:Sub"},
		{"thematic break is not setext", "text\n\n---\n## h\n", "2:h"},
		{"html comment hides", "<!--\n## in\n-->\n## out\n", "2:out"},
		{"quote and list", "> ## q\n- ## l\n", "2:q|2:l"},
		{"crlf", "# a\r\n## b\r\n```\r\n## in\r\n```\r\n", "1:a|2:b"},
		{"lone angle bracket line", "<\na\n<\n> <\n- <\n## h\n", "2:h"},
	}
	for _, c := range cases {
		var got []string
		for _, h := range Headings([]byte(c.in)) {
			got = append(got, fmt.Sprintf("%d:%s", h.Level, h.Text))
		}
		if g := strings.Join(got, "|"); g != c.want {
			t.Errorf("%s: %q -> %q, want %q", c.name, c.in, g, c.want)
		}
	}
}

func BenchmarkBuildVersion(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		_ = BuildVersion()
	}
}

func TestOneLineDropsTerminalControls(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                      "plain",
		"a\n\tb  c":                  "a b c",
		"x\x1b[2Jy\x1b]52;c;Zm9v\ay": "x[2Jy]52;c;Zm9vy",
		"del\x7fete":                 "delete",
	} {
		if got := OneLine(in); got != want {
			t.Errorf("OneLine(%q) = %q, want %q", in, got, want)
		}
	}
}
