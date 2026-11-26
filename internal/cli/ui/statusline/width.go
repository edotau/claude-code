package statusline

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/cli/ui/ansi"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// termWidth is the terminal probe; a var so tests never walk real processes.
var termWidth = ancestorTTYWidth

// termWidthTTL lets a resize show within seconds while sparing a process walk per render.
const termWidthTTL = 5 * time.Second

// ccMargin is the columns Claude Code keeps around the statusline: a 2-cell indent plus 2 at the right.
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

// clip truncates each line to width visible cells, keeping SGR escapes and ending in "…".
func clip(s string, width int) string {
	if width <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = textutil.TruncateCells(l, width, "…")
		if lines[i] != l && !strings.Contains(lines[i], "\x1b") { // keep the reset a clipped plain line always got
			lines[i] += ansi.Reset
		}
	}
	return strings.Join(lines, "\n")
}

// longestLine is the widest line of s in visible cells.
func longestLine(s string) int {
	w := 0
	for l := range strings.SplitSeq(s, "\n") {
		w = max(w, textutil.DisplayWidth(l))
	}
	return w
}

// walkAncestors climbs at most 8 hops from pid to the first tty whose width cols reports, all within one budget.
func walkAncestors(pid int, budget time.Duration, lookup func(context.Context, int) (int, string, error),
	cols func(dev string) int) int {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	for hop := 0; hop < 8 && pid > 1; hop++ {
		ppid, tty, err := lookup(ctx, pid)
		if err != nil {
			return 0
		}
		if tty != "" && !strings.HasPrefix(tty, "?") && tty != "-" {
			if w := cols("/dev/" + tty); w > 0 {
				return w
			}
		}
		pid = ppid
	}
	return 0
}
