package tui

import (
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// palette is the theme source; tests swap it for a fixed Palette or ok=false.
var palette = theme.Current

// colorMode is truecolor or ansi256 from the environment; "" (16-color, NO_COLOR, unknown) keeps the legacy codes.
func colorMode() string {
	if os.Getenv("NO_COLOR") != "" {
		return ""
	}
	if ct := os.Getenv("COLORTERM"); ct == "truecolor" || ct == "24bit" {
		return ansi.TrueColor
	}
	if strings.Contains(os.Getenv("TERM"), "256color") {
		return ansi.ANSI256
	}
	return ""
}

// themed returns the palette and mode when both are usable.
func themed() (theme.Palette, string, bool) {
	mode := colorMode()
	if mode == "" {
		return theme.Palette{}, "", false
	}
	p, ok := palette()
	return p, mode, ok
}

// Accent paints s bold in the theme accent; without a palette or colour mode it is bold cyan.
func Accent(on bool, s string) string {
	if p, mode, ok := themed(); on && ok && s != "" {
		return ansi.Paint(mode, p.Accent, s, true)
	}
	return ansi.Style(on, ansi.Bold+ansi.Cyan, s)
}

// AccentMark paints s (the cursor mark) in the theme accent, not bold; the fallback is cyan.
func AccentMark(on bool, s string) string {
	if p, mode, ok := themed(); on && ok && s != "" {
		return ansi.Paint(mode, p.Accent, s, false)
	}
	return ansi.Style(on, ansi.Cyan, s)
}

// Selected paints s on the theme selection background; the fallback is reverse video.
func Selected(on bool, s string) string {
	if p, mode, ok := themed(); on && ok && s != "" {
		return ansi.BgSeq(mode, p.Selection) + s + ansi.Reset
	}
	return ansi.Style(on, "\x1b[7m", s)
}
