package statusline

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// cell is one output rune tagged with how it is colored: g|sep|dot|filled|empty|dim|pad.
type cell struct {
	ch   rune
	kind string
}

// sepPath is the fixed path→body separator.
const sepPath = " | "

// easeAt bends a ramp position t∈[0,1] by exp (t^exp); exp ≤ 0 or 1 leaves it linear.
func easeAt(t, exp float64) float64 {
	if exp <= 0 || exp == 1 {
		return t
	}
	return math.Pow(t, exp)
}

// renderGradient paints "user:" then sweeps the ramp from the path across cfg.Order, the branch and the
// velocity counts; separators, empty cells and the dot keep their own tones.
func renderGradient(v values, cfg Config, pal theme.Palettes, stops []ansi.RGB, mode string) string {
	dim := ansi.Hex(pal.Sem("dim"))
	cells := gradientCells(v, cfg)
	total := max(len(cells)-1, 1)
	var out strings.Builder
	if cfg.seg("path", true) {
		if cfg.UserColor == "ansi" {
			out.WriteString("\x1b[01;32m" + v.userHost + ansi.Reset)
		} else {
			out.WriteString(ansi.Paint(mode, ansi.Hex(pal.Sem("user")), v.userHost, true))
		}
		out.WriteString(ansi.Paint(mode, ansi.Hex(pal.Sem("punct")), ":", false))
	}
	for i, c := range cells {
		rgb := ansi.GradAt(stops, easeAt(float64(i)/float64(total), cfg.GradientEase))
		switch c.kind {
		case "sep", "empty": // empty ─ cells take the separator dim: the trough tone all but vanished on them
			out.WriteString(ansi.Paint(mode, dim, string(c.ch), false))
		case "dot":
			out.WriteString(ansi.Paint(mode, ansi.TowardWhite(rgb, 0.45), string(c.ch), true))
		default:
			out.WriteString(ansi.Paint(mode, rgb, string(c.ch), true))
		}
	}
	if cfg.seg("velocity", true) && (v.adds != 0 || v.dels != 0) {
		out.WriteString(ansi.Paint(mode, dim, sepPath, false))
		out.WriteString(velocityText(v, mode, pal, "/"))
	}
	return out.String()
}

// cellBuf accumulates the gradient line's characters with their paint kinds.
type cellBuf struct{ cells []cell }

func (b *cellBuf) push(s, kind string) {
	for _, ch := range s {
		b.cells = append(b.cells, cell{ch, kind})
	}
}

// pushSep pushes a separator, tagging the dot and bar glyphs so they take their own tone.
func (b *cellBuf) pushSep(s string) {
	for _, ch := range s {
		kind := "g"
		switch ch {
		case '●':
			kind = "dot"
		case '|', '│':
			kind = "sep"
		}
		b.cells = append(b.cells, cell{ch, kind})
	}
}

// pushContext pushes twoline's "ctx <bar> NN.N% · used/size" block; the bar's filled and empty runs keep their tones.
func (b *cellBuf) pushContext(v values) {
	d := v.two
	if !d.hasCtx {
		return
	}
	bar := twoBar(d.ctxPct, 10)
	filled := strings.TrimRight(bar, "─")
	b.push("ctx ", "g")
	b.push(filled, "filled")
	b.push(bar[len(filled):], "empty")
	b.push(fmt.Sprintf(" %.1f%%", d.ctxPct)+twoSep+compactTokens(d.tokens)+"/"+compactTokens(d.size), "g")
	if v.limits != "" {
		b.push(" "+v.limits, "g") // 7d:N% on a subscription, else the token countdown
	}
}

func gradientCells(v values, cfg Config) []cell {
	var b cellBuf
	if cfg.seg("path", true) {
		b.push(v.path, "g")
		b.pushSep(sepPath)
	}
	b.pushMetrics(v, cfg)
	return b.cells
}

// pushMetrics pushes enabled cfg.Order segments but path, provider, limits and total: the context block after the
// major separator, the rest as text joined by the minor separator.
func (b *cellBuf) pushMetrics(v values, cfg Config) {
	sepMajor := cfg.sep("major", "  ●  ")
	sepMinor := cfg.sep("minor", " · ")
	emitted := false
	for _, key := range cfg.Order {
		if key == "path" || key == "provider" || key == "limits" || key == "total" || !cfg.seg(key, true) {
			continue
		}
		if key == "context" {
			b.pushSep(sepMajor)
			b.pushContext(v)
			emitted = true
			continue
		}
		text := v.text(key)
		if text == "" {
			continue
		}
		if emitted {
			b.pushSep(sepMinor)
		}
		b.push(text, "g")
		emitted = true
	}
}

// velocityText paints "+A<sep>-D" in the add/del tones, omitting a zero side.
func velocityText(v values, mode string, pal theme.Palettes, sep string) string {
	var parts []string
	if v.adds != 0 {
		parts = append(parts, ansi.Paint(mode, ansi.Hex(pal.Sem("add")), "+"+strconv.Itoa(v.adds), false))
	}
	if v.dels != 0 {
		parts = append(parts, ansi.Paint(mode, ansi.Hex(pal.Sem("del")), "-"+strconv.Itoa(v.dels), false))
	}
	return strings.Join(parts, sep)
}

// contextBarParts returns the filled and empty runs separately so renderers classify cells by construction.
func contextBarParts(pct int, bar BarConfig) (filledPart, emptyPart string) {
	width := bar.Width
	if width <= 0 {
		width = 8
	}
	filled, empty := bar.FilledGlyph, bar.EmptyGlyph
	if filled == "" {
		filled = "█"
	}
	if empty == "" {
		empty = "▒"
	}
	n := max(0, min(width, int(float64(pct)/100*float64(width)+0.5)))
	return strings.Repeat(filled, n), strings.Repeat(empty, width-n)
}

// truncRunes clips b to n runes with a trailing "…"; n<=0 leaves it whole.
func truncRunes(b string, n int) string {
	if n <= 0 || utf8.RuneCountInString(b) <= n {
		return b
	}
	return string([]rune(b)[:max(1, n-1)]) + "…"
}

// troughTone derives empty-bar cells from the terminal bg (not the ramp, which drags troughs dark):
// dark lifts toward white, light toward black, by emptyContrast.
func troughTone(bar BarConfig) ansi.RGB {
	bg := bar.TerminalBg
	if bg == "" {
		bg = "#300a24"
	}
	termBg := ansi.Hex(bg)
	theme := bar.Theme
	if theme == "" || theme == "auto" {
		theme = "light"
		if 0.2126*float64(termBg[0])+0.7152*float64(termBg[1])+0.0722*float64(termBg[2]) < 128 {
			theme = "dark"
		}
	}
	if theme == "dark" {
		return ansi.TowardWhite(termBg, bar.EmptyContrast)
	}
	return ansi.Mix(termBg, ansi.RGB{0, 0, 0}, bar.EmptyContrast)
}
