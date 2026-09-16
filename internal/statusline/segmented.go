package statusline

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/ansi"
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

func themeColors(cfg Config, pal Palettes) map[string]map[string]string {
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

// renderSegmented renders the powerline, capsule, or minimal style over cfg.Order then git.
func renderSegmented(v values, cfg Config, pal Palettes, mode string) string {
	var order []string
	for _, k := range cfg.Order {
		if k != "path" && cfg.seg(k, true) {
			order = append(order, k)
		}
	}
	if cfg.seg("git", true) && v.branch != "" {
		order = append(order, "git")
	}
	th := themeColors(cfg, pal)

	var blocks []block
	for _, key := range order {
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
	if len(blocks) == 0 {
		return ""
	}

	var out strings.Builder
	if cfg.Style == "capsule" {
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
	} else {
		renderBlocks(&out, blocks, cfg, pal, mode)
	}
	if cfg.seg("velocity", true) && (v.adds != 0 || v.dels != 0) {
		out.WriteString(" " + velocityText(v, mode, pal))
	}
	return out.String()
}

// renderBlocks joins powerline blocks with arrows (nerd charset) or minimal blocks with the minor separator.
func renderBlocks(out *strings.Builder, blocks []block, cfg Config, pal Palettes, mode string) {
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
			out.WriteString(ansi.Paint(mode, ansi.Hex(pal.sem("dim")), cfg.sep("minor", " · "), false))
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
func dashboardValueCells(col string, v values, cfg Config) (string, []cell) {
	switch col {
	case "context":
		filled, empty := contextBarParts(v.contextPct, cfg.Bar)
		cells := append(runeCells(filled, "filled"), runeCells(empty, "empty")...)
		return col, append(cells, runeCells(" "+strconv.Itoa(v.contextPct)+"%", "g")...)
	case "workspace":
		if v.path == "" {
			return "", nil
		}
		return col, runeCells(v.path, "g")
	case "branch":
		if v.branch == "" {
			return "", nil
		}
		return col, runeCells(truncBranch(v.branch, cfg.BranchMax), "g")
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

// renderDashboard renders the two-line column dashboard; the head opens the value row and a same-width
// indent keeps the header row aligned over it.
func renderDashboard(v values, cfg Config, pal Palettes, stops []ansi.RGB, mode string, width int) string {
	dash := cfg.Dashboard
	cols := dash.Columns
	if len(cols) == 0 {
		cols = []string{"workspace", "model", "provider", "router", "branch", "context"}
	}
	gutter := dash.Gutter
	if gutter <= 0 {
		gutter = 3
	}
	head := promptHead(v, cfg, pal, stops, mode)
	indentW := 0
	if head != "" {
		indentW = headWidth(v, cfg) + utf8.RuneCountInString(sepPath)
	}
	W := dash.Width
	if W <= 0 {
		W = 100
		if width > 0 {
			W = width - indentW
		}
	}

	var built []builtCol
	for _, c := range cols {
		if c == "workspace" && head != "" { // the head already carries the path
			continue
		}
		if hdr, cells := dashboardValueCells(c, v, cfg); cells != nil {
			built = append(built, builtCol{c, hdr, cells})
		}
	}
	if len(built) == 0 {
		return head
	}
	hdr, val := layoutDashboard(built, gutter, W)
	dim := ansi.Hex(pal.sem("dim"))
	trough := troughTone(cfg.Bar)
	total := max(len(val)-1, 1)
	var vb strings.Builder
	if head != "" {
		vb.WriteString(head + ansi.Paint(mode, dim, sepPath, false))
	}
	for i, c := range val {
		rgb := ansi.GradAt(stops, float64(i)/float64(total))
		if c.kind == "empty" {
			rgb = ansi.Mix(rgb, trough, 0.84)
		}
		vb.WriteString(ansi.Paint(mode, rgb, string(c.ch), c.kind != "empty"))
	}
	if !dash.Headers {
		return vb.String()
	}
	var hb strings.Builder
	hb.WriteString(strings.Repeat(" ", indentW))
	for _, c := range hdr {
		if c.kind == "dim" {
			hb.WriteString(ansi.Paint(mode, dim, string(c.ch), false))
		} else {
			hb.WriteString(string(c.ch))
		}
	}
	return hb.String() + "\n" + vb.String()
}
