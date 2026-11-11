package statusline

import (
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ui/ansi"
)

// termWidth is the terminal probe; a var so tests never walk real processes.
var termWidth = ancestorTTYWidth

// termWidthTTL lets a resize show within seconds while sparing a process walk per render.
const termWidthTTL = 5 * time.Second

// ccMargin is the columns Claude Code keeps around the statusline (2-cell indent + 2 at the right: 81 of 85 cols shown).
const ccMargin = 4

// lineWidth is config "width", else $COLUMNS or the nearest ancestor terminal's width less ccMargin, else 0 (no clip).
func lineWidth(cfg Config) int {
	if cfg.Width > 0 {
		return cfg.Width
	}
	return max(termColumns()-ccMargin, 0)
}

// termColumns is $COLUMNS, else the nearest ancestor terminal's width (cached), else 0.
func termColumns() int {
	if n := ansi.Columns(0); n > 0 {
		return n
	}
	stamp := stampFile("termwidth", strconv.Itoa(os.Getppid()))
	if f := freshStamp(stamp, termWidthTTL, 1); f != nil {
		n, _ := strconv.Atoi(f[0])
		return n
	}
	n := max(termWidth(), 0)
	writeTimedStamp(stamp, strconv.Itoa(n))
	return n
}

// clip truncates each line to width visible cells (see cellWidth), keeping SGR escapes and ending in "…".
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

// forEachCell walks s calling fn per SGR escape (w == 0) or rune (w cells); fn false stops. A bare ESC is skipped.
func forEachCell(s string, fn func(tok string, w int) bool) {
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				i++
				continue
			}
			if !fn(s[i:i+j+1], 0) {
				return
			}
			i += j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if !fn(string(r), cellWidth(r)) {
			return
		}
		i += size
	}
}

func clipLine(s string, width int) string {
	if visibleWidth(s) <= width {
		return s
	}
	var b strings.Builder
	cells := 0
	forEachCell(s, func(tok string, w int) bool {
		if w > 0 && cells+w > width-1 {
			b.WriteString("…")
			return false
		}
		b.WriteString(tok)
		cells += w
		return true
	})
	return b.String() + ansi.Reset
}

func visibleWidth(s string) int {
	n := 0
	forEachCell(s, func(_ string, w int) bool { n += w; return true })
	return n
}

// cellWidth is the terminal columns r occupies: 2 for East Asian wide and fullwidth runes, else 1.
func cellWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6, r >= 0x1F300 && r <= 0x1F64F, r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

// longestLine is the widest line of s in visible cells.
func longestLine(s string) int {
	w := 0
	for l := range strings.SplitSeq(s, "\n") {
		w = max(w, visibleWidth(l))
	}
	return w
}
