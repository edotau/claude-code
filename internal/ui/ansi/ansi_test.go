package ansi

import (
	"math"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest" // tests refuse real DNS: mock API calls
	_ "github.com/edotau/claude-code/internal/nonet"
)

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

func TestToANSI256ExactLightGray(t *testing.T) {
	if got := ToANSI256(RGB{238, 238, 238}); got != 255 {
		t.Fatalf("ToANSI256({238 238 238}) = %d, want exact grayscale index 255", got)
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

func TestStatusPaintsTokensOnlyWhenEnabled(t *testing.T) {
	line := "codex      present (ChatGPT login) · cred:ok · ABSENT"
	if got := Status(false, line); got != line {
		t.Errorf("Status(off) changed the text: %q", got)
	}
	got := Status(true, line)
	for _, want := range []string{Green + "present" + Reset, Green + "cred:ok" + Reset, Red + "ABSENT" + Reset} {
		if !strings.Contains(got, want) {
			t.Errorf("Status(on) missing %q in %q", want, got)
		}
	}
	if Style(true, Bold+Cyan, "x") != Bold+Cyan+"x"+Reset || Style(false, Red, "x") != "x" {
		t.Error("Style wraps only when on")
	}
}

func TestSweepColorsEachRuneAcrossTheGradient(t *testing.T) {
	stops := []RGB{{0, 0, 0}, {200, 100, 50}}
	want := FgSeq(TrueColor, stops[0], true) + "a" + FgSeq(TrueColor, stops[1], true) + "b" + Reset
	if got := Sweep(TrueColor, stops, "ab"); got != want {
		t.Errorf("Sweep = %q, want %q", got, want)
	}
	if got, want := Sweep(TrueColor, stops, "é"), FgSeq(TrueColor, stops[0], true)+"é"+Reset; got != want {
		t.Errorf("single rune Sweep = %q, want %q", got, want)
	}
}

func TestGradAtEndpoints(t *testing.T) {
	stops := []RGB{{0, 0, 0}, {255, 255, 255}}
	if got := GradAt(stops, 0); got != (RGB{0, 0, 0}) {
		t.Errorf("GradAt frac=0 = %v, want black", got)
	}
	if got := GradAt(stops, 1); got != (RGB{255, 255, 255}) {
		t.Errorf("GradAt frac=1 = %v, want white", got)
	}
	// Out of range clamps rather than indexing past the ramp.
	if got := GradAt(stops, -3); got != (RGB{0, 0, 0}) {
		t.Errorf("GradAt frac=-3 = %v, want black", got)
	}
	if got := GradAt(stops, 9); got != (RGB{255, 255, 255}) {
		t.Errorf("GradAt frac=9 = %v, want white", got)
	}
	// single stop → flat color at any fraction
	flat := []RGB{{10, 20, 30}}
	if got := GradAt(flat, 0.5); got != (RGB{10, 20, 30}) {
		t.Errorf("GradAt single-stop = %v, want flat", got)
	}
	// no stops → neutral gray, never panics
	if got := GradAt(nil, 0.5); got != (RGB{150, 150, 160}) {
		t.Errorf("GradAt no-stops = %v, want neutral gray", got)
	}
}

func TestMixAndTowardWhite(t *testing.T) {
	if got := Mix(RGB{0, 0, 0}, RGB{255, 255, 255}, 0.5); got != (RGB{128, 128, 128}) {
		t.Errorf("Mix midpoint = %v, want 128 (round-half-away-from-zero)", got)
	}
	if got := Mix(RGB{10, 20, 30}, RGB{200, 200, 200}, 0); got != (RGB{10, 20, 30}) {
		t.Errorf("Mix w=0 = %v, want the left stop", got)
	}
	if got := Mix(RGB{10, 20, 30}, RGB{200, 200, 200}, 1); got != (RGB{200, 200, 200}) {
		t.Errorf("Mix w=1 = %v, want the right stop", got)
	}
	if got := TowardWhite(RGB{0, 100, 200}, 0.5); got != (RGB{128, 178, 228}) {
		t.Errorf("TowardWhite 50%% = %v, want {128 178 228}", got)
	}
	if got := TowardWhite(RGB{7, 8, 9}, 1); got != (RGB{255, 255, 255}) {
		t.Errorf("TowardWhite w=1 = %v, want white", got)
	}
}

// legacySpan is the span selector the cli package carried in its own sampler before both callers were
// folded onto GradAt: a linear scan comparing t against each boundary, where GradAt divides. The two
// forms disagree the moment a float lands a hair either side of a boundary, and the symptom would be a
// silently repainted sweep — nothing downstream compares an escape against the ramp it came from.
func legacySpan(n int, t float64) int {
	step := 100 / float64(n-1)
	k := 0
	for k < n-2 && t >= float64(k+1)*step {
		k++
	}
	return k
}

// gradAtSpan mirrors GradAt's own selector so a sweep can compare the two decisions directly.
func gradAtSpan(n int, t float64) int {
	step := 100 / float64(n-1)
	return min(int(t/step), n-2)
}

func TestGradAtSpanMatchesTheLegacyScan(t *testing.T) {
	for n := 2; n <= 6; n++ {
		stops := make([]RGB, n)
		for i := range stops {
			stops[i] = RGB{i * 37 % 256, 255 - i*29%256, i * 61 % 256}
		}
		check := func(frac float64) {
			clamped := math.Max(0, math.Min(100, frac*100))
			want := legacySpan(n, clamped)
			if got := gradAtSpan(n, clamped); got != want {
				t.Fatalf("n=%d frac=%.17g: span %d, legacy scan says %d", n, frac, got, want)
			}
			// The span index is the only thing that differed; prove the sampled color agrees too.
			a, b := stops[want], stops[want+1]
			step := 100 / float64(n-1)
			if w := GradAt(stops, frac); w != Mix(a, b, smooth((clamped-float64(want)*step)/step)) {
				t.Fatalf("n=%d frac=%.17g: GradAt = %v, legacy path = %v",
					n, frac, w, Mix(a, b, smooth((clamped-float64(want)*step)/step)))
			}
		}
		// Every exact span boundary and its immediate float neighbours — where the two forms can part.
		for k := 0; k <= n-1; k++ {
			b := float64(k) / float64(n-1)
			for _, f := range []float64{b, math.Nextafter(b, 0), math.Nextafter(b, 1)} {
				check(f)
			}
		}
		for _, f := range []float64{-1, -0.0001, 0, 1, 1.0001, 2} {
			check(f)
		}
		const steps = 20001
		for i := range steps {
			check(float64(i) / float64(steps-1))
		}
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
