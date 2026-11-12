package statusline

import (
	"cmp"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/theme"
)

// twoData is the payload facts the twoline style needs beyond values; zero fields drop their cell.
type twoData struct {
	model, effort, auth string
	ctxPct              float64
	size, tokens        int
	hasCtx              bool
	week                int // 7d used percent; -1 when absent
}

// twoSeg is one painted cell: key picks its powerline.minimal colors ("" = no background).
type twoSeg struct {
	key, text, fg string // fg: fixed hex that overrides the ramp
	bold, dim     bool
	bare          bool // padding or the head gap: never takes a derived background
}

// collectTwoline resolves model, effort, context and 7d from the payload; auth is the collected countdown.
func collectTwoline(in input, auth string) twoData {
	d := twoData{model: in.Model.DisplayName, week: limitsFromPayload(in), auth: auth}
	if in.Effort != nil {
		d.effort = in.Effort.Level
	}
	d.fillContext(in)
	return d
}

// fillContext sets the context facts (percent, size, used tokens) from the payload's context_window.
func (d *twoData) fillContext(in input) {
	if cw := in.ContextWindow; cw != nil {
		d.hasCtx, d.size, d.tokens = true, cw.Size, cw.TotalInput
		transcript.RecordWindow(in.SessionID, cw.Size) // the Stop hook's context share uses this size
		d.ctxPct = float64(contextPct(in))
		if cw.UsedPercentage != nil {
			d.ctxPct = *cw.UsedPercentage
		}
	}
}

// twoBar is w cells of ━ (filled) then ─ for pct percent, rounded (p*w+50)/100.
func twoBar(pct float64, w int) string {
	n := (clampPct(int(pct+0.5))*w + 50) / 100
	return strings.Repeat("━", n) + strings.Repeat("─", w-n)
}

const twoSep = " · "

// twolineRows builds row 1 after the head (provider, auth, model, effort, velocity) and row 2 (context, tokens, 7d).
func twolineRows(v values) (row1, row2 []twoSeg) {
	d := v.two
	if v.provider != "" {
		row1 = append(row1, twoSeg{key: "cwd", text: v.provider})
	}
	if d.auth != "" {
		row1 = appendSep(row1, twoSeg{key: "auth", text: d.auth})
	}
	row1 = appendSep(row1, twoSeg{key: "model", text: dropDatabricksClaude(cmp.Or(d.model, v.model)), bold: true})
	if d.effort != "" {
		row1 = append(row1, twoSeg{key: "effort", text: ":" + d.effort, dim: true})
	}
	row1 = appendVelocity(row1, v.adds, v.dels)
	if d.hasCtx {
		ctx := "ctx " + twoBar(d.ctxPct, 20) + fmt.Sprintf(" %d%%", int(math.Round(d.ctxPct)))
		toks := textutil.CompactCount(int64(d.tokens)) + "/" + textutil.CompactCount(int64(d.size))
		row2 = appendSep(row2, twoSeg{key: "context", text: ctx})
		row2 = appendSep(row2, twoSeg{key: "tokens", text: toks})
	}
	if d.week >= 0 {
		row2 = appendSep(row2, twoSeg{key: "limit7d", text: fmt.Sprintf("7d %s %d%%", twoBar(float64(d.week), 10), d.week)})
	}
	return row1, row2
}

// appendVelocity adds " | +A -D" after row 1's model and effort, omitting a zero side and the cell when both are 0.
func appendVelocity(row []twoSeg, adds, dels int) []twoSeg {
	if adds == 0 && dels == 0 {
		return row
	}
	row = append(row, twoSeg{key: "sep", text: sepPath})
	if adds != 0 {
		row = append(row, twoSeg{key: "add", text: "+" + strconv.Itoa(adds)})
	}
	if adds != 0 && dels != 0 {
		row = append(row, twoSeg{text: " "})
	}
	if dels != 0 {
		row = append(row, twoSeg{key: "del", text: "-" + strconv.Itoa(dels)})
	}
	return row
}

// appendSep appends s after a separator when row already has cells.
func appendSep(row []twoSeg, s twoSeg) []twoSeg {
	if len(row) > 0 {
		row = append(row, twoSeg{key: "sep", text: twoSep})
	}
	return append(row, s)
}

// twoShade is how far a derived cell background darkens its ramp color toward black.
const twoShade = 0.8

// paintTwoRow sweeps the ramp across the row's runes; a cell the palette colors gets its themed fg and bg.
func paintTwoRow(row []twoSeg, th map[string]map[string]string, stops []ansi.RGB, mode string) string {
	total := 0
	for _, s := range row {
		total += utf8.RuneCountInString(s.text)
	}
	var b strings.Builder
	i := 0
	for _, s := range row {
		if bg := th[s.key]["bg"]; bg != "" {
			b.WriteString(ansi.BgSeq(mode, ansi.Hex(bg)))
		} else if len(th) == 0 && !s.bare { // ramp-only palette: back each cell with a dark shade of its ramp color
			mid := float64(i) + float64(utf8.RuneCountInString(s.text)-1)/2
			b.WriteString(ansi.BgSeq(mode, ansi.Mix(ansi.GradAt(stops, mid/float64(max(total-1, 1))), ansi.RGB{}, twoShade)))
		}
		if s.dim || s.key == "sep" {
			b.WriteString("\x1b[2m")
		}
		for _, r := range s.text {
			rgb := ansi.GradAt(stops, float64(i)/float64(max(total-1, 1)))
			if s.key == "sep" {
				rgb = ansi.Hex(cmp.Or(th["sep"]["fg"], "#CAD3F5"))
			} else if s.fg != "" {
				rgb = ansi.Hex(s.fg)
			} else if fg := th[s.key]["fg"]; fg != "" { // a palette with cell colors (minimal) sets fg with its bg
				rgb = ansi.Hex(fg)
			}
			b.WriteString(ansi.FgSeq(mode, rgb, s.bold))
			b.WriteString(string(r))
			i++
		}
		b.WriteString(ansi.Reset)
	}
	return b.String()
}

// renderTwoline draws the two-row style, both rows left-packed so a resize clips only their ends.
func renderTwoline(v values, cfg config, pal theme.Palettes, stops []ansi.RGB, mode string) string {
	th := pal.Powerline[cfg.Gradient] // background blocks only with a palette that defines them (minimal)
	row1, row2 := twolineRows(v)
	for i := range row1 { // velocity keeps the add/del tones every style uses
		if k := row1[i].key; k == "add" || k == "del" {
			row1[i].fg = pal.Sem(k)
		}
	}
	out := paintTwoRow(row1, th, stops, mode)
	if head := promptHead(v, cfg, pal, stops, mode); head != "" { // the PS1 head leads
		out = head + paintTwoRow([]twoSeg{{key: "sep", bare: true, text: sepPath}}, th, stops, mode) + out
	}
	if len(row2) == 0 {
		return out
	}
	return out + "\n" + paintTwoRow(row2, th, stops, mode)
}
