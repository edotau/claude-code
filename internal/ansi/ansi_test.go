package ansi

import "testing"

func TestHex(t *testing.T) {
	cases := []struct {
		in   string
		want RGB
	}{
		{"#FF6B9D", RGB{255, 107, 157}},
		{"4796E4", RGB{71, 150, 228}}, // the leading # is optional
		{"#abc", RGB{170, 187, 204}},  // 3-digit shorthand doubles each nibble
		{"garbage", RGB{0, 0, 0}},     // wrong length → black, never a panic
		{"", RGB{0, 0, 0}},
	}
	for _, c := range cases {
		if got := Hex(c.in); got != c.want {
			t.Errorf("Hex(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseHex(t *testing.T) {
	cases := []struct {
		in    string
		rgb   RGB
		alpha int
		ok    bool
	}{
		{"#300a24", RGB{0x30, 0x0a, 0x24}, 255, true},
		{"abc", RGB{170, 187, 204}, 255, true},
		{"#abcd", RGB{170, 187, 204}, 221, true}, // 4-digit shorthand doubles the alpha nibble too
		{"#ffffff80", RGB{255, 255, 255}, 128, true},
		{"#zzzzzz", RGB{}, 0, false}, // a non-hex digit rejects the whole value, never a partial parse
		{"#12345", RGB{}, 0, false},
		{"", RGB{}, 0, false},
	}
	for _, c := range cases {
		rgb, alpha, ok := ParseHex(c.in)
		if rgb != c.rgb || alpha != c.alpha || ok != c.ok {
			t.Errorf("ParseHex(%q) = %v,%d,%v want %v,%d,%v", c.in, rgb, alpha, ok, c.rgb, c.alpha, c.ok)
		}
	}
}

// Hex keeps its no-failure-channel contract: the shapes ParseHex accepts but Hex never did stay black.
func TestHexStaysBlackOutsideItsTwoShapes(t *testing.T) {
	for _, in := range []string{"#ffffff80", "#abcd", "#1g3456"} {
		if got := Hex(in); got != (RGB{}) {
			t.Errorf("Hex(%q) = %v, want black", in, got)
		}
	}
}

func TestToANSI256(t *testing.T) {
	cases := []struct {
		in   RGB
		want int
	}{
		{RGB{0, 0, 0}, 16},        // black → cube origin
		{RGB{255, 255, 255}, 231}, // white → cube max
		{RGB{128, 128, 128}, 244}, // mid gray → grayscale ramp
	}
	for _, c := range cases {
		if got := ToANSI256(c.in); got != c.want {
			t.Errorf("ToANSI256(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestToANSI16Nearest(t *testing.T) {
	// Pure red should map to a red index (1 or its bright variant 9).
	if got := ToANSI16(RGB{255, 0, 0}); got != 1 && got != 9 {
		t.Errorf("ToANSI16(red) = %d, want 1 or 9", got)
	}
	if got := ToANSI16(RGB{0, 0, 0}); got != 0 {
		t.Errorf("ToANSI16(black) = %d, want 0", got)
	}
	if got := ToANSI16(RGB{255, 255, 255}); got != 15 {
		t.Errorf("ToANSI16(white) = %d, want 15", got)
	}
}

// TestFgSeqPerMode pins every branch of the mode switch. The default arm downsamples to 16 colors, so a
// caller passing a mode label that does not match these consts loses truecolor with no other symptom.
func TestFgSeqPerMode(t *testing.T) {
	cases := []struct {
		mode string
		rgb  RGB
		bold bool
		want string
	}{
		{TrueColor, RGB{1, 2, 3}, false, "\x1b[38;2;1;2;3m"},
		{TrueColor, RGB{1, 2, 3}, true, "\x1b[1;38;2;1;2;3m"},
		{ANSI256, RGB{128, 128, 128}, false, "\x1b[38;5;244m"},
		{ANSI256, RGB{128, 128, 128}, true, "\x1b[1;38;5;244m"},
		// A dim index carries no bright prefix; bold only adds the 1; attribute.
		{ANSI16, RGB{255, 0, 0}, false, "\x1b[31m"},
		{ANSI16, RGB{255, 0, 0}, true, "\x1b[1;31m"},
		// Indices ≥ 8 are reached via the bright prefix, which bold already supplies — so the bold and
		// non-bold escapes for a bright color are deliberately the same bytes.
		{ANSI16, RGB{255, 255, 255}, false, "\x1b[1;37m"},
		{ANSI16, RGB{255, 255, 255}, true, "\x1b[1;37m"},
		{"nonsense", RGB{255, 0, 0}, false, "\x1b[31m"}, // unknown mode falls back to 16 colors
	}
	for _, c := range cases {
		if got := FgSeq(c.mode, c.rgb, c.bold); got != c.want {
			t.Errorf("FgSeq(%q, %v, bold=%v) = %q, want %q", c.mode, c.rgb, c.bold, got, c.want)
		}
	}
}

func TestBgSeqPerMode(t *testing.T) {
	cases := []struct {
		mode string
		rgb  RGB
		want string
	}{
		{TrueColor, RGB{1, 2, 3}, "\x1b[48;2;1;2;3m"},
		{ANSI256, RGB{128, 128, 128}, "\x1b[48;5;244m"},
		{ANSI16, RGB{255, 0, 0}, "\x1b[41m"},
		{ANSI16, RGB{255, 255, 255}, "\x1b[47m"}, // bright index folds onto its dim base
	}
	for _, c := range cases {
		if got := BgSeq(c.mode, c.rgb); got != c.want {
			t.Errorf("BgSeq(%q, %v) = %q, want %q", c.mode, c.rgb, got, c.want)
		}
	}
}

// TestPaintAlwaysResets: painted cells are concatenated, so a missing reset bleeds one color into every
// following segment of the line.
func TestPaintAlwaysResets(t *testing.T) {
	got := Paint(TrueColor, RGB{1, 2, 3}, "x", false)
	if want := "\x1b[38;2;1;2;3mx" + Reset; got != want {
		t.Errorf("Paint = %q, want %q", got, want)
	}
	if Reset != "\x1b[0m" {
		t.Errorf("Reset = %q, want the all-attributes-off escape", Reset)
	}
}

// TestModeConstsAreTheWireStrings: the mode travels as a plain string through config JSON and
// COLORTERM sniffing. Renaming a const value silently routes every render to the 16-color arm.
func TestModeConstsAreTheWireStrings(t *testing.T) {
	for got, want := range map[string]string{TrueColor: "truecolor", ANSI256: "ansi256", ANSI16: "ansi16"} {
		if got != want {
			t.Errorf("mode const = %q, want %q", got, want)
		}
	}
}
