package statusline

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ansi"
)

// cell is one output rune tagged with how it is colored: g|sep|dot|filled|empty|dim|pad.
type cell struct {
	ch   rune
	kind string
}

// sepPath is the fixed path→body separator.
const sepPath = " | "

// renderGradient paints "user:" then sweeps the ramp from the path across cfg.Order, the branch and the
// velocity counts; separators, troughs and the dot keep their own tones.
func renderGradient(v values, cfg Config, pal Palettes, stops []ansi.RGB, mode string) string {
	dim := ansi.Hex(pal.sem("dim"))
	sepMajor := cfg.sep("major", "  ●  ")
	sepMinor := cfg.sep("minor", " · ")
	sepGit := cfg.sep("git", " │ ")

	var cells []cell
	push := func(s, kind string) {
		for _, ch := range s {
			cells = append(cells, cell{ch, kind})
		}
	}
	pushSep := func(s string) {
		for _, ch := range s {
			switch ch {
			case '●':
				cells = append(cells, cell{ch, "dot"})
			case '|', '│':
				cells = append(cells, cell{ch, "sep"})
			default:
				cells = append(cells, cell{ch, "g"})
			}
		}
	}

	if cfg.seg("path", true) {
		push(v.path, "g")
		pushSep(sepPath)
	}
	emitted := false
	for _, key := range cfg.Order {
		if key == "path" || !cfg.seg(key, true) {
			continue
		}
		if key == "context" {
			pushSep(sepMajor)
			filled, empty := contextBarParts(v.contextPct, cfg.Bar)
			push(filled, "filled")
			push(empty, "empty")
			if cfg.Bar.Percent != "before" {
				push(" "+strconv.Itoa(v.contextPct)+"%", "g")
			}
			emitted = true
			continue
		}
		text := v.text(key)
		if text == "" {
			continue
		}
		if emitted {
			pushSep(sepMinor)
		}
		push(text, "g")
		emitted = true
	}
	if cfg.seg("git", true) && v.branch != "" {
		push(sepGit, "sep")
		push(truncBranch(v.branch, cfg.BranchMax), "g")
	}

	total := max(len(cells)-1, 1)
	trough := troughTone(cfg.Bar)
	var out strings.Builder
	if cfg.seg("path", true) {
		if cfg.UserColor == "ansi" {
			out.WriteString("\x1b[01;32m" + v.userHost + ansi.Reset)
		} else {
			out.WriteString(ansi.Paint(mode, ansi.Hex(pal.sem("user")), v.userHost, true))
		}
		out.WriteString(ansi.Paint(mode, ansi.Hex(pal.sem("punct")), ":", false))
	}
	for i, c := range cells {
		rgb := ansi.GradAt(stops, float64(i)/float64(total))
		switch c.kind {
		case "sep":
			out.WriteString(ansi.Paint(mode, dim, string(c.ch), false))
		case "empty":
			out.WriteString(ansi.Paint(mode, trough, string(c.ch), false))
		case "dot":
			out.WriteString(ansi.Paint(mode, ansi.TowardWhite(rgb, 0.45), string(c.ch), true))
		default:
			out.WriteString(ansi.Paint(mode, rgb, string(c.ch), true))
		}
	}
	if cfg.seg("velocity", true) && (v.adds != 0 || v.dels != 0) {
		out.WriteString(ansi.Paint(mode, dim, sepMinor, false))
		out.WriteString(velocityText(v, mode, pal))
	}
	return out.String()
}

// velocityText paints "+A -D" in the add/del tones, omitting a zero side.
func velocityText(v values, mode string, pal Palettes) string {
	var parts []string
	if v.adds != 0 {
		parts = append(parts, ansi.Paint(mode, ansi.Hex(pal.sem("add")), "+"+strconv.Itoa(v.adds), false))
	}
	if v.dels != 0 {
		parts = append(parts, ansi.Paint(mode, ansi.Hex(pal.sem("del")), "-"+strconv.Itoa(v.dels), false))
	}
	return strings.Join(parts, " ")
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

// truncBranch clips b to n runes with a trailing "…"; n<=0 leaves it whole.
func truncBranch(b string, n int) string {
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
