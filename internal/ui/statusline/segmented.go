package statusline

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// Nerd Font glyphs: powerline right-arrow, left/right round caps.
const (
	glyphArrow = ""
	glyphCapL  = ""
	glyphCapR  = ""
)

// fallbackTheme backs a missing powerline theme (the palette's "dark").
var fallbackTheme = map[string]map[string]string{
	"directory": {"bg": "#3B4252", "fg": "#ECEFF4"},
	"git":       {"bg": "#434C5E", "fg": "#A3BE8C"},
	"model":     {"bg": "#5E81AC", "fg": "#ECEFF4"},
	"context":   {"bg": "#4C566A", "fg": "#88C0D0"},
	"session":   {"bg": "#2E3440", "fg": "#D8DEE9"},
}

func themeColors(cfg config, pal theme.Palettes) map[string]map[string]string {
	name := cfg.PowerlineTheme
	if name == "" {
		name = "dark"
	}
	if th, ok := pal.Powerline[name]; ok {
		return th
	}
	return fallbackTheme
}

// segLabel is a segment's padded block text in the segmented styles; "" drops the block.
func segLabel(key string, v values) string {
	switch key {
	case "git":
		return "  " + v.branch + " "
	case "context":
		return " " + strconv.Itoa(v.contextPct) + "% "
	}
	if s := v.text(key); s != "" {
		return " " + s + " "
	}
	return ""
}

// segmentOrder is the configured order minus path, plus git last when there is a branch and no configured slot.
func segmentOrder(v values, cfg config) []string {
	var order []string
	for _, k := range cfg.Order {
		if k != "path" && cfg.seg(k, true) {
			order = append(order, k)
		}
	}
	if cfg.seg("git", true) && v.branch != "" && !slices.Contains(order, "git") {
		order = append(order, "git")
	}
	return order
}

// segmentBlocks resolves each non-empty segment to its themed block.
func segmentBlocks(v values, cfg config, pal theme.Palettes) []block {
	th := themeColors(cfg, pal)
	var blocks []block
	for _, key := range segmentOrder(v, cfg) {
		txt := segLabel(key, v)
		if strings.TrimSpace(txt) == "" {
			continue
		}
		c, ok := th[key]
		if !ok {
			c = map[string]string{"bg": "#4C566A", "fg": "#ECEFF4"}
		}
		blocks = append(blocks, block{ansi.Hex(c["bg"]), ansi.Hex(c["fg"]), txt})
	}
	return blocks
}

// renderSegmented renders the powerline, capsule, or minimal style over cfg.Order, then git.
func renderSegmented(v values, cfg config, pal theme.Palettes, mode string) string {
	blocks := segmentBlocks(v, cfg, pal)
	if len(blocks) == 0 {
		return ""
	}

	var out strings.Builder
	if cfg.Style == "capsule" {
		renderCapsules(&out, blocks, cfg, mode)
	} else {
		renderBlocks(&out, blocks, cfg, pal, mode)
	}
	if cfg.seg("velocity", true) && (v.adds != 0 || v.dels != 0) {
		out.WriteString(" " + velocityText(v, mode, pal))
	}
	return out.String()
}

// renderCapsules writes each block between round caps (parentheses in the text charset), space-separated.
func renderCapsules(out *strings.Builder, blocks []block, cfg config, mode string) {
	capL, capR := glyphCapL, glyphCapR
	if cfg.Charset == "text" {
		capL, capR = "(", ")"
	}
	for i, b := range blocks {
		if i > 0 {
			out.WriteString(" ")
		}
		out.WriteString(ansi.FgSeq(mode, b.bg, false) + capL + ansi.Reset +
			ansi.BgSeq(mode, b.bg) + ansi.FgSeq(mode, b.fg, true) + b.txt + ansi.Reset +
			ansi.FgSeq(mode, b.bg, false) + capR + ansi.Reset)
	}
}

// renderBlocks joins powerline blocks with arrows (nerd charset) or minimal blocks with the minor separator.
func renderBlocks(out *strings.Builder, blocks []block, cfg config, pal theme.Palettes, mode string) {
	arrow := ""
	if cfg.Style == "powerline" && cfg.Charset != "text" {
		arrow = glyphArrow
	}
	for i, b := range blocks {
		out.WriteString(ansi.BgSeq(mode, b.bg) + ansi.FgSeq(mode, b.fg, true) + b.txt + ansi.Reset)
		switch {
		case cfg.Style == "powerline" && i+1 < len(blocks):
			out.WriteString(ansi.BgSeq(mode, blocks[i+1].bg) + ansi.FgSeq(mode, b.bg, false) + arrow + ansi.Reset)
		case cfg.Style == "powerline":
			out.WriteString(ansi.FgSeq(mode, b.bg, false) + arrow + ansi.Reset)
		case cfg.Style == "minimal" && i+1 < len(blocks):
			out.WriteString(ansi.Paint(mode, ansi.Hex(pal.Sem("dim")), cfg.sep("minor", " · "), false))
		}
	}
}

// block is one segmented-style segment: colors and padded text.
type block struct {
	bg, fg ansi.RGB
	txt    string
}

// builtCol is a resolved dashboard column: header text and value-row cells.
type builtCol struct {
	name  string
	hdr   string
	cells []cell
}

func runeCells(s, kind string) []cell {
	cells := make([]cell, 0, len(s))
	for _, ch := range s {
		cells = append(cells, cell{ch, kind})
	}
	return cells
}

// dashboardValueCells returns (header, cells) for one column, or nil cells to drop it.
func dashboardValueCells(col string, v values, cfg config) (string, []cell) {
	switch col {
	case "context":
		filled, empty := contextBarParts(v.contextPct, cfg.Bar)
		bar := append(runeCells(filled, "filled"), runeCells(empty, "empty")...)
		pct := strconv.Itoa(v.contextPct) + "%"
		if cfg.Bar.Percent == "before" {
			return col, append(runeCells(pct+" ", "g"), bar...)
		}
		return col, append(bar, runeCells(" "+pct, "g")...)
	case "workspace":
		if v.path == "" {
			return "", nil
		}
		return col, runeCells(v.path, "g")
	case "branch":
		if v.branch == "" {
			return "", nil
		}
		return col, runeCells(v.branch, "g")
	}
	if txt := v.text(col); txt != "" {
		return col, runeCells(txt, "g")
	}
	return "", nil
}

// layoutDashboard aligns header + value rows: left columns padded to max(header,value) and joined by the
// gutter, then the context column right-aligned to W.
func layoutDashboard(built []builtCol, gutter, W int) (hdr, val []cell) {
	var left []builtCol
	var ctxCol *builtCol
	for i := range built {
		if built[i].name == "context" {
			ctxCol = &built[i]
		} else {
			left = append(left, built[i])
		}
	}
	pad := func(row []cell, n int, kind string) []cell {
		for ; n > 0; n-- {
			row = append(row, cell{' ', kind})
		}
		return row
	}
	for idx, bc := range left {
		hw := utf8.RuneCountInString(bc.hdr)
		colW := max(hw, len(bc.cells))
		hdr = pad(append(hdr, runeCells(bc.hdr, "dim")...), colW-hw, "pad")
		val = pad(append(val, bc.cells...), colW-len(bc.cells), "g")
		if idx < len(left)-1 || ctxCol != nil {
			hdr, val = pad(hdr, gutter, "pad"), pad(val, gutter, "g")
		}
	}
	if ctxCol != nil {
		hw := utf8.RuneCountInString(ctxCol.hdr)
		colW := max(hw, len(ctxCol.cells))
		filler := max(W-len(val)-colW, 1)
		hdr, val = pad(hdr, filler, "pad"), pad(val, filler, "g")
		hdr = append(pad(hdr, colW-hw, "pad"), runeCells(ctxCol.hdr, "dim")...)
		val = append(pad(val, colW-len(ctxCol.cells), "g"), ctxCol.cells...)
	}
	return hdr, val
}

// dashboardColumns is the configured dashboard columns, or the default five.
func dashboardColumns(dash dashboardConfig) []string {
	if len(dash.Columns) == 0 {
		return []string{"workspace", "model", "provider", "branch", "context"}
	}
	return dash.Columns
}

// dashboardWidth is the row width: the configured one, else the terminal width less the head indent, else 100.
func dashboardWidth(dash dashboardConfig, width, indentW int) int {
	if dash.Width > 0 {
		return dash.Width
	}
	if width > 0 {
		return width - indentW
	}
	return 100
}

// buildDashboardCols resolves cols to non-empty columns; the workspace column drops when a head carries the path.
func buildDashboardCols(cols []string, v values, cfg config, hasHead bool) []builtCol {
	var built []builtCol
	for _, c := range cols {
		if c == "workspace" && hasHead || !cfg.seg(c, true) { // the head already carries the path; disabled drops out
			continue
		}
		if hdr, cells := dashboardValueCells(c, v, cfg); cells != nil {
			built = append(built, builtCol{c, hdr, cells})
		}
	}
	return built
}

// paintDashboardValues paints the value row with the gradient, behind the head when present.
func paintDashboardValues(val []cell, head string, cfg config, pal theme.Palettes, stops []ansi.RGB, mode string) string {
	trough := troughTone(cfg.Bar)
	total := max(len(val)-1, 1)
	var vb strings.Builder
	if head != "" {
		vb.WriteString(head + ansi.Paint(mode, ansi.Hex(pal.Sem("dim")), sepPath, false))
	}
	for i, c := range val {
		rgb := ansi.GradAt(stops, float64(i)/float64(total))
		if c.kind == "empty" {
			rgb = ansi.Mix(rgb, trough, 0.84)
		}
		vb.WriteString(ansi.Paint(mode, rgb, string(c.ch), c.kind != "empty"))
	}
	return vb.String()
}

// paintDashboardHeaders paints the header row, dimming header text, under an indentW indent.
func paintDashboardHeaders(hdr []cell, indentW int, pal theme.Palettes, mode string) string {
	dim := ansi.Hex(pal.Sem("dim"))
	var hb strings.Builder
	hb.WriteString(strings.Repeat(" ", indentW))
	for _, c := range hdr {
		if c.kind == "dim" {
			hb.WriteString(ansi.Paint(mode, dim, string(c.ch), false))
		} else {
			hb.WriteString(string(c.ch))
		}
	}
	return hb.String()
}

// renderDashboard renders the two-line column dashboard; the head opens the value row and a same-width
// indent keeps the header row aligned over it.
func renderDashboard(v values, cfg config, pal theme.Palettes, stops []ansi.RGB, mode string, width int) string {
	dash := cfg.Dashboard
	gutter := dash.Gutter
	if gutter <= 0 {
		gutter = 3
	}
	head := promptHead(v, cfg, pal, stops, mode)
	indentW := 0
	if head != "" {
		indentW = headWidth(v, cfg) + utf8.RuneCountInString(sepPath)
	}
	built := buildDashboardCols(dashboardColumns(dash), v, cfg, head != "")
	if len(built) == 0 {
		return head
	}
	hdr, val := layoutDashboard(built, gutter, dashboardWidth(dash, width, indentW))
	row := paintDashboardValues(val, head, cfg, pal, stops, mode)
	if !dash.Headers {
		return row
	}
	return paintDashboardHeaders(hdr, indentW, pal, mode) + "\n" + row
}

// promptUser is PS1's \u; the hostname is dead weight on a single-machine bar.
func promptUser() string {
	if user := os.Getenv("USER"); user != "" {
		return user
	}
	return "user"
}

// promptHead is the segmented styles' "user:path" head: literal PS1 bold-green/bold-blue under
// userColor "ansi", else the palette user tone and the ramp's opening stop. Gradient draws its own.
func promptHead(v values, cfg config, pal theme.Palettes, stops []ansi.RGB, mode string) string {
	if !cfg.seg("path", true) {
		return ""
	}
	if cfg.UserColor == "ansi" {
		return "\x1b[01;32m" + v.userHost + ansi.Reset + ":" + "\x1b[01;34m" + v.path + ansi.Reset
	}
	return ansi.Paint(mode, ansi.Hex(pal.Sem("user")), v.userHost, true) +
		ansi.Paint(mode, ansi.Hex(pal.Sem("punct")), ":", false) +
		ansi.Paint(mode, ansi.GradAt(stops, 0), v.path, true)
}

// headWidth is the head's printable width, so the dashboard header row can sit over the shifted values.
func headWidth(v values, cfg config) int {
	if !cfg.seg("path", true) {
		return 0
	}
	return utf8.RuneCountInString(v.userHost) + 1 + utf8.RuneCountInString(v.path)
}
