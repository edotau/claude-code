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

// renderGradient paints "user:" and the ramp; a segment that would cross width starts the next row whole.
func renderGradient(v values, cfg config, pal theme.Palettes, stops []ansi.RGB, mode string, width int) string {
	dim := ansi.Hex(pal.Sem("dim"))
	b := gradientCells(v, cfg)
	var out strings.Builder
	out.Grow(len(b.cells)*24 + 64)
	col := writeUserHost(&out, v, cfg, pal, mode)
	col = wrapSegments(&out, b.cells, b.brks, stops, cfg.GradientEase, mode, dim, col, width)
	if cfg.seg("velocity") && (v.adds != 0 || v.dels != 0) {
		writeVelocity(&out, velocityText(v, mode, pal), ansi.Paint(mode, dim, sepPath, false), col, width)
	}
	return out.String()
}

// writeUserHost paints "user:" when the path shows and returns the columns it took.
func writeUserHost(out *strings.Builder, v values, cfg config, pal theme.Palettes, mode string) int {
	if !cfg.seg("path") {
		return 0
	}
	if cfg.UserColor == "ansi" {
		out.WriteString("\x1b[01;32m")
		out.WriteString(v.userHost)
		out.WriteString(ansi.Reset)
	} else {
		out.WriteString(ansi.Paint(mode, ansi.Hex(pal.Sem("user")), v.userHost, true))
	}
	out.WriteString(ansi.Paint(mode, ansi.Hex(pal.Sem("punct")), ":", false))
	return visibleWidth(v.userHost) + 1
}

// writeVelocity appends +A/-D after sep, or alone on a new row when it would cross width.
func writeVelocity(out *strings.Builder, vel, sep string, col, width int) {
	if width > 0 && col > 0 && col+len(sepPath)+visibleWidth(vel) > width {
		out.WriteByte('\n')
	} else {
		out.WriteString(sep)
	}
	out.WriteString(vel)
}

func appendPaintedCell(buf []byte, c cell, mode string, dim, rgb ansi.RGB) []byte {
	switch c.kind {
	case "sep", "empty": // empty ─ cells take the separator dim: the trough tone all but vanished on them
		buf = ansi.AppendFgSeq(buf, mode, dim, false)
	case "dot":
		buf = ansi.AppendFgSeq(buf, mode, ansi.TowardWhite(rgb, 0.45), true)
	default:
		buf = ansi.AppendFgSeq(buf, mode, rgb, true)
	}
	buf = utf8.AppendRune(buf, c.ch)
	return append(buf, ansi.Reset...)
}

// wrapSegments writes cells from col, breaking before a segment that crosses width (0: never); widths are cells.
func wrapSegments(out *strings.Builder, cells []cell, brks []brk, stops []ansi.RGB, ease float64, mode string, dim ansi.RGB, col, width int) int {
	next := 0
	total := float64(max(len(cells)-1, 1))
	var buf [48]byte
	for i := 0; i < len(cells); {
		if next < len(brks) && brks[next].at == i {
			sep := brks[next]
			next++
			end := len(cells)
			if next < len(brks) {
				end = brks[next].at
			}
			if width > 0 && col > 0 && col+cellsWidth(cells[sep.at:end]) > width {
				out.WriteByte('\n')
				col, i = 0, sep.at+sep.n
				continue
			}
		}
		rgb := ansi.GradAt(stops, easeAt(float64(i)/total, ease))
		out.Write(appendPaintedCell(buf[:0], cells[i], mode, dim, rgb))
		col += cellWidth(cells[i].ch)
		i++
	}
	return col
}

// cellsWidth is the terminal columns the cells occupy.
func cellsWidth(cells []cell) int {
	n := 0
	for _, c := range cells {
		n += cellWidth(c.ch)
	}
	return n
}

// cellBuf accumulates the gradient line's characters with their paint kinds.
type cellBuf struct {
	cells []cell
	brks  []brk // where a row may break: each separator between segments
}

// brk is a separator of n cells starting at cell at; a row break replaces it.
type brk struct{ at, n int }

// pushBreakable pushes separator s as a place the line may wrap.
func (b *cellBuf) pushBreakable(s, kind string) {
	b.brks = append(b.brks, brk{len(b.cells), utf8.RuneCountInString(s)})
	b.push(s, kind)
}

func (b *cellBuf) push(s, kind string) {
	for _, ch := range s {
		b.cells = append(b.cells, cell{ch, kind})
	}
}

// pushSep pushes a separator, tagging the dot and bar glyphs so they take their own tone.
func (b *cellBuf) pushSep(s string) {
	b.brks = append(b.brks, brk{len(b.cells), utf8.RuneCountInString(s)})
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

// pushContextBlock pushes the context block, led by the major separator if something precedes it; false if none.
func (b *cellBuf) pushContextBlock(v values, sepMajor string, lead bool) bool {
	if !v.two.hasCtx {
		return false
	}
	if lead {
		b.pushSep(sepMajor)
	}
	d := v.two
	bar := twoBar(d.ctxPct, 10)
	b.push("ctx ", "g")
	filled := strings.TrimRight(bar, "─")
	b.push(filled, "filled")
	b.push(bar[len(filled):], "empty")
	b.push(fmt.Sprintf(" %d%%", clampPct(int(math.Round(d.ctxPct)))), "g")
	b.pushBreakable(twoSep, "g")
	b.push(tokenCount(d.tokens)+"/"+tokenCount(d.size), "g")
	if v.limits != "" {
		b.pushBreakable(" ", "g")
		b.push(v.limits, "g")
	}
	return true
}

func gradientCells(v values, cfg config) cellBuf {
	b := cellBuf{
		cells: make([]cell, 0, 64),
		brks:  make([]brk, 0, 8),
	}
	if cfg.seg("path") {
		b.push(v.path, "g")
		b.pushSep(sepPath)
	}
	b.pushMetrics(v, cfg)
	return b
}

// pushMetrics pushes the enabled segments but path, provider, limits and total: context block first, the rest as text.
func (b *cellBuf) pushMetrics(v values, cfg config) {
	sepMajor := cfg.sep("major", "  ●  ")
	sepMinor := cfg.sep("minor", " · ")
	emitted := false
	for _, key := range cfg.Order {
		if gradientSkips(key) || !cfg.seg(key) {
			continue
		}
		if key == "context" {
			emitted = b.pushContextBlock(v, sepMajor, emitted) || emitted
			continue
		}
		for _, text := range gradientTexts(v, key) {
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
}

// gradientTexts is a segment's text; the model carries the profile.
func gradientTexts(v values, key string) []string {
	if key == "model" && !v.two.hasCtx {
		return []string{v.model, v.profile}
	}
	if key == "model" {
		return []string{v.model, v.profile}
	}
	return []string{v.text(key)}
}

// gradientSkips names the segments the gradient line paints elsewhere or never.
func gradientSkips(key string) bool {
	return key == "path" || key == "provider" || key == "limits" || key == "total"
}

// velocityText paints "+A -D" in the add/del tones, omitting a zero side.
func velocityText(v values, mode string, pal theme.Palettes) string {
	var parts []string
	if v.adds != 0 {
		parts = append(parts, ansi.Paint(mode, ansi.Hex(pal.Sem("add")), "+"+strconv.Itoa(v.adds), false))
	}
	if v.dels != 0 {
		parts = append(parts, ansi.Paint(mode, ansi.Hex(pal.Sem("del")), "-"+strconv.Itoa(v.dels), false))
	}
	return strings.Join(parts, " ")
}

// maxBarWidth caps a configured bar so a typo cannot allocate without bound on every render.
const maxBarWidth = 200

// contextBarParts returns the filled and empty runs separately so renderers classify cells by construction.
func contextBarParts(pct int, bar barConfig) (filledPart, emptyPart string) {
	width := bar.Width
	if width <= 0 {
		width = 8
	}
	width = min(width, maxBarWidth)
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

// troughTone derives empty-bar cells from the terminal bg (not the ramp, which drags troughs dark) by emptyContrast.
func troughTone(bar barConfig) ansi.RGB {
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
