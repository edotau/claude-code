package statusline

import (
	"os"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ansi"
)

// promptUser is PS1's \u; the hostname is dead weight on a single-machine bar.
func promptUser() string {
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return "user"
}

// promptHead is the segmented styles' "user:path" head: literal PS1 bold-green/bold-blue under
// userColor "ansi", else the palette user tone and the ramp's opening stop. Gradient draws its own.
func promptHead(v values, cfg Config, pal Palettes, stops []ansi.RGB, mode string) string {
	if !cfg.seg("path", true) {
		return ""
	}
	if cfg.UserColor == "ansi" {
		return "\x1b[01;32m" + v.userHost + ansi.Reset + ":" + "\x1b[01;34m" + v.path + ansi.Reset
	}
	return ansi.Paint(mode, ansi.Hex(pal.sem("user")), v.userHost, true) +
		ansi.Paint(mode, ansi.Hex(pal.sem("punct")), ":", false) +
		ansi.Paint(mode, ansi.GradAt(stops, 0), v.path, true)
}

// headWidth is the head's printable width, so the dashboard header row can sit over the shifted values.
func headWidth(v values, cfg Config) int {
	if !cfg.seg("path", true) {
		return 0
	}
	return utf8.RuneCountInString(v.userHost) + 1 + utf8.RuneCountInString(v.path)
}
