package statusline

import (
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ansi"
)

// lineWidth is config "width", else $COLUMNS when the terminal exported it, else 0 (no clip).
func lineWidth(cfg Config) int {
	if cfg.Width > 0 {
		return cfg.Width
	}
	n, _ := strconv.Atoi(os.Getenv("COLUMNS"))
	return max(n, 0)
}

// clip truncates each line to width visible cells (one per rune), keeping SGR escapes and ending in "…".
func clip(s string, width int) string {
	if width <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = clipLine(l, width)
	}
	return strings.Join(lines, "\n")
}

func clipLine(s string, width int) string {
	if visibleWidth(s) <= width {
		return s
	}
	var b strings.Builder
	cells := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				break
			}
			b.WriteString(s[i : i+j+1])
			i += j + 1
			continue
		}
		if cells == width-1 {
			b.WriteString("…")
			break
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b.WriteRune(r)
		cells++
		i += size
	}
	return b.String() + ansi.Reset
}

func visibleWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				return n
			}
			i += j + 1
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		n++
		i += size
	}
	return n
}
