package statusline

import (
	"testing"

	"github.com/edotau/claude-code/internal/ui/ansi"
)

func near(a, b ansi.RGB) bool {
	for i := range a {
		if d := a[i] - b[i]; d < -1 || d > 1 {
			return false
		}
	}
	return true
}

func TestLiftIdentityAtZero(t *testing.T) {
	in := ansi.Hex("#7C6C7C")
	if got := (Config{}).lifted(in); got != in {
		t.Fatalf("lift 0 changed %v to %v", in, got)
	}
	pal := embeddedPalettes()
	if got := (Config{}).liftPalette(pal); got.sem("dim") != pal.sem("dim") {
		t.Fatalf("lift 0 changed dim: %s", got.sem("dim"))
	}
}

func TestLiftKnownValues(t *testing.T) {
	c := Config{Lift: 0.15}
	for hex, want := range map[string]string{"#7C6C7C": "#908291", "#4796E4": "#62A6E8"} {
		if got := c.lifted(ansi.Hex(hex)); !near(got, ansi.Hex(want)) {
			t.Errorf("%s: got %v want %v", hex, got, ansi.Hex(want))
		}
	}
	pal := embeddedPalettes()
	if got := c.liftPalette(pal).sem("dim"); !near(ansi.Hex(got), c.lifted(ansi.Hex(pal.sem("dim")))) {
		t.Errorf("palette dim = %s", got)
	}
}

func TestLiftGeminiStops(t *testing.T) {
	base := resolveGradient(Config{Gradient: "gemini"}, embeddedPalettes())
	got := resolveGradient(Config{Gradient: "gemini", Lift: 0.15}, embeddedPalettes())
	for i := range base {
		for ch := range base[i] {
			if got[i][ch] < base[i][ch] || got[i][ch] > 255 {
				t.Fatalf("stop %d ch %d: %d -> %d", i, ch, base[i][ch], got[i][ch])
			}
		}
		if (base[i][0] > base[i][1]) != (got[i][0] > got[i][1]) && base[i][0] != base[i][1] {
			t.Errorf("stop %d channel order flipped: %v -> %v", i, base[i], got[i])
		}
	}
}

func TestLiftClamped(t *testing.T) {
	in := ansi.Hex("#4796E4")
	if got, want := (Config{Lift: 5}).lifted(in), (Config{Lift: 0.6}).lifted(in); got != want {
		t.Errorf("high not clamped: %v vs %v", got, want)
	}
	if got := (Config{Lift: -1}).lifted(in); got != in {
		t.Errorf("negative not clamped: %v", got)
	}
}
