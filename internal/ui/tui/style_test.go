package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/theme"
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
		if got, want := accentMark(on, "x"), ansi.Style(on, ansi.Cyan, "x"); got != want {
			t.Errorf("accentMark(%v) = %q, want %q", on, got, want)
		}
		if got, want := selected(on, "x"), ansi.Style(on, "\x1b[7m", "x"); got != want {
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
	if got := accentMark(true, "x"); !strings.Contains(got, "38;2;71;150;228") {
		t.Errorf("accentMark = %q, want Border fg", got)
	}
	if got := selected(true, "x"); !strings.Contains(got, "48;2;45;74;122") || strings.Contains(got, "\x1b[7m") {
		t.Errorf("Selected = %q, want Sel bg and no reverse video", got)
	}
	for name, got := range map[string]string{"Accent": Accent(false, "x"), "accentMark": accentMark(false, "x"),
		"Selected": selected(false, "x")} {
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
	if got, want := selected(true, "x"), ansi.Style(true, "\x1b[7m", "x"); got != want {
		t.Errorf("NO_COLOR Selected = %q, want %q", got, want)
	}
}

var csiRE = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func visible(s string) string { return csiRE.ReplaceAllString(s, "") }

// TestMuteDetailBadgeAccentOnlyWithColor: the badge is wrapped when colour is on and the text never changes.
func TestMuteDetailBadgeAccentOnlyWithColor(t *testing.T) {
	setPalette(t, theme.Palette{Accent: ansi.RGB{71, 150, 228}}, true)
	setTerm(t, "truecolor", "xterm")
	row := "Label  some detail  [active]"
	if got := muteDetail(false, row); got != row {
		t.Errorf("colour off = %q, want the row unchanged", got)
	}
	got := muteDetail(true, row)
	if want := accentMark(true, "[active") + accentMark(true, "]"); !strings.HasSuffix(got, want) {
		t.Errorf("badge not wrapped in the accent: %q", got)
	}
	if visible(got) != row {
		t.Errorf("stripped = %q, want %q", visible(got), row)
	}
	if got := muteDetail(true, "Label  no badge"); strings.Contains(got, "38;2;71;150;228") {
		t.Errorf("a row without a badge must not carry the accent: %q", got)
	}
}

// TestRenderRowsFitEightyColumns: every laid-out row stays within 80 columns with colour on and off.
func TestRenderRowsFitEightyColumns(t *testing.T) {
	setPalette(t, theme.Palette{Accent: ansi.RGB{71, 150, 228}, Selection: ansi.RGB{45, 74, 122}}, true)
	setTerm(t, "truecolor", "xterm")
	rows := layoutRows([][2]string{
		{"Subscription", "OAuth · plan limits · Remote Control [not logged in]"},
		{"API key", "paste a key and keep it in the vault for every agent session [verified]"},
		{"Back", "return to the previous menu"},
	}, 78)
	for _, color := range []bool{false, true} {
		for i, line := range strings.Split(render("T", pickState{rows: rows, sel: 1}, color), "\r\n") {
			if n := len([]rune(visible(line))); n > 80 {
				t.Errorf("color=%v line %d is %d columns: %q", color, i, n, visible(line))
			}
		}
	}
}
