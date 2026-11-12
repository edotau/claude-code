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
func lineWidth(cfg config) int {
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

// forEachCell walks s calling fn per CSI escape (w == 0) or rune (w cells); fn false stops. A bare ESC is skipped.
func forEachCell(s string, fn func(tok string, w int) bool) {
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			end := csiEnd(s, i)
			if end < 0 {
				i++
				continue
			}
			if !fn(s[i:end], 0) {
				return
			}
			i = end
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if !fn(string(r), cellWidth(r)) {
			return
		}
		i += size
	}
}

// csiEnd is the index after the CSI sequence (ESC [ params final) starting at s[i], or -1 when s[i:] is not one.
func csiEnd(s string, i int) int {
	if i+1 >= len(s) || s[i+1] != '[' {
		return -1
	}
	for j := i + 2; j < len(s); j++ {
		if s[j] >= 0x40 && s[j] <= 0x7e {
			return j + 1
		}
	}
	return -1
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

// zeroWidth lists combining marks, ZWSP/ZWNJ/ZWJ and directional marks, word joiners and variation selectors.
var zeroWidth = [][2]rune{
	{0x0300, 0x036F}, {0x1AB0, 0x1AFF}, {0x1DC0, 0x1DFF}, {0x200B, 0x200F}, {0x2060, 0x2064},
	{0x20D0, 0x20FF}, {0xFE00, 0xFE0F}, {0xFE20, 0xFE2F}, {0xE0100, 0xE01EF},
}

// wideCells lists East Asian wide/fullwidth runes and the emoji with default emoji presentation.
var wideCells = [][2]rune{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3},
	{0x25FD, 0x25FE}, {0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693},
	{0x26A1, 0x26A1}, {0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE},
	{0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA},
	{0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728}, {0x274C, 0x274C},
	{0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0},
	{0x27BF, 0x27BF}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x2E80, 0xA4CF},
	{0xAC00, 0xD7A3}, {0xF900, 0xFAFF}, {0xFE30, 0xFE4F}, {0xFF00, 0xFF60}, {0xFFE0, 0xFFE6},
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A},
	{0x1F300, 0x1F64F}, {0x1F680, 0x1F6FF}, {0x1F7E0, 0x1F7EB}, {0x1F900, 0x1F9FF},
	{0x1FA70, 0x1FAFF}, {0x20000, 0x3FFFD},
}

func inRanges(r rune, table [][2]rune) bool {
	for _, t := range table {
		if r >= t[0] && r <= t[1] {
			return true
		}
	}
	return false
}

// cellWidth is the terminal columns r occupies: 0 for combining/joiner/variation runes, 2 for wide and emoji, else 1.
func cellWidth(r rune) int {
	switch {
	case inRanges(r, zeroWidth):
		return 0
	case inRanges(r, wideCells):
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
