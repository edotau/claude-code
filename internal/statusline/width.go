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
				// malformed/incomplete escape: skip just the ESC byte, keep counting the rest
				i++
				continue
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
				// malformed/incomplete escape: skip just the ESC byte, keep counting the rest
				i++
				continue
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

// longestLine is the widest line of s in visible cells.
func longestLine(s string) int {
	w := 0
	for _, l := range strings.Split(s, "\n") {
		w = max(w, visibleWidth(l))
	}
	return w
}
