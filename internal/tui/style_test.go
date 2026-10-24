package tui

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/theme"
)

func setPalette(t *testing.T, p theme.Palette, ok bool) {
	t.Helper()
	orig := palette
	t.Cleanup(func() { palette = orig })
	palette = func() (theme.Palette, bool) { return p, ok }
}

func setTerm(t *testing.T, colorterm, term string) {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	t.Setenv("COLORTERM", colorterm)
	t.Setenv("TERM", term)
}

// TestStylesFallbackByteIdentical: with no palette the output is exactly the legacy cyan / reverse video.
func TestStylesFallbackByteIdentical(t *testing.T) {
	setPalette(t, theme.Palette{}, false)
	setTerm(t, "truecolor", "xterm-256color")
	for _, on := range []bool{true, false} {
		if got, want := Accent(on, "x"), ansi.Style(on, ansi.Bold+ansi.Cyan, "x"); got != want {
			t.Errorf("Accent(%v) = %q, want %q", on, got, want)
		}
		if got, want := AccentMark(on, "x"), ansi.Style(on, ansi.Cyan, "x"); got != want {
			t.Errorf("AccentMark(%v) = %q, want %q", on, got, want)
		}
		if got, want := Selected(on, "x"), ansi.Style(on, "\x1b[7m", "x"); got != want {
			t.Errorf("Selected(%v) = %q, want %q", on, got, want)
		}
	}
}

// TestStylesTruecolorUsePalette: a fixed palette in truecolor emits Border's fg and Sel's bg sequences.
func TestStylesTruecolorUsePalette(t *testing.T) {
	setPalette(t, theme.Palette{Accent: ansi.RGB{71, 150, 228}, Selection: ansi.RGB{45, 74, 122}}, true)
	setTerm(t, "truecolor", "xterm")
	if got := Accent(true, "x"); !strings.Contains(got, "38;2;71;150;228") || !strings.Contains(got, "x") {
		t.Errorf("Accent = %q, want Border 38;2;71;150;228", got)
	}
	if got := AccentMark(true, "x"); !strings.Contains(got, "38;2;71;150;228") {
		t.Errorf("AccentMark = %q, want Border fg", got)
	}
	if got := Selected(true, "x"); !strings.Contains(got, "48;2;45;74;122") || strings.Contains(got, "\x1b[7m") {
		t.Errorf("Selected = %q, want Sel bg and no reverse video", got)
	}
	for name, got := range map[string]string{"Accent": Accent(false, "x"), "AccentMark": AccentMark(false, "x"),
		"Selected": Selected(false, "x")} {
		if got != "x" {
			t.Errorf("%s with on=false = %q, want plain text", name, got)
		}
	}
}

// TestStylesNoColorOrDumbTermKeepLegacy: NO_COLOR or a 16-color TERM ignores the palette.
func TestStylesNoColorOrDumbTermKeepLegacy(t *testing.T) {
	setPalette(t, theme.Palette{Accent: ansi.RGB{1, 2, 3}, Selection: ansi.RGB{4, 5, 6}}, true)
	setTerm(t, "", "xterm")
	if got, want := Accent(true, "x"), ansi.Style(true, ansi.Bold+ansi.Cyan, "x"); got != want {
		t.Errorf("16-color Accent = %q, want %q", got, want)
	}
	setTerm(t, "truecolor", "xterm")
	t.Setenv("NO_COLOR", "1")
	if got, want := Selected(true, "x"), ansi.Style(true, "\x1b[7m", "x"); got != want {
		t.Errorf("NO_COLOR Selected = %q, want %q", got, want)
	}
}
