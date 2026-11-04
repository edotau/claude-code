package statusline

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
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
}

// collectTwoline resolves model, effort, auth countdown, context and 7d from the payload.
func collectTwoline(in Input, session string) twoData {
	d := twoData{model: in.Model.DisplayName, week: limitsFromPayload(in)}
	if in.Effort != nil {
		d.effort = in.Effort.Level
	}
	if !onSubscription(session) {
		if exp, ok := providers.TokenExpiry(session); ok {
			d.auth = "auth:" + strings.Replace(tokenLeft(tokenLabel(session), exp, time.Now()), ":", " ", 1)
		}
	}
	if cw := in.ContextWindow; cw != nil {
		d.hasCtx, d.size, d.tokens = true, cw.Size, cw.TotalInput
		d.ctxPct = float64(contextPct(in))
		if cw.UsedPercentage != nil {
			d.ctxPct = *cw.UsedPercentage
		}
	}
	return d
}

// compactTokens is 950, 94.4k, 200k, 1.2M.
func compactTokens(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return strings.Replace(fmt.Sprintf("%.1fk", float64(n)/1000), ".0k", "k", 1)
	}
	return strings.Replace(fmt.Sprintf("%.1fM", float64(n)/1e6), ".0M", "M", 1)
}

// twoBar is w cells of ━ (filled) then ─ for pct percent, rounded (p*w+50)/100.
func twoBar(pct float64, w int) string {
	n := (clampPct(int(pct+0.5))*w + 50) / 100
	return strings.Repeat("━", n) + strings.Repeat("─", w-n)
}

const twoSep = " · "

// twolineRows builds row 1 after the head (branch, model, effort, velocity) and row 2 (auth, context, tokens, 7d) as segment lists.
func twolineRows(v values) (row1, row2 []twoSeg) {
	d := v.two
	if v.provider != "" {
		row1 = append(row1, twoSeg{key: "cwd", text: v.provider + " "})
	}
	if v.branch != "" {
		row1 = append(row1, twoSeg{key: "branch", text: ":" + v.branch})
	}
	row1 = append(row1, twoSeg{key: "sep", text: twoSep}, twoSeg{key: "model", text: cmpModel(d.model, v.model), bold: true})
	if d.effort != "" {
		row1 = append(row1, twoSeg{key: "effort", text: " / " + d.effort, dim: true})
	}
	row1 = appendVelocity(row1, v.adds, v.dels)
	if d.auth != "" {
		row2 = append(row2, twoSeg{text: d.auth})
	}
	if d.hasCtx {
		row2 = appendSep(row2, twoSeg{key: "context", text: "ctx " + twoBar(d.ctxPct, 20) + fmt.Sprintf(" %.1f%%", d.ctxPct)})
		row2 = appendSep(row2, twoSeg{key: "tokens", text: compactTokens(d.tokens) + "/" + compactTokens(d.size)})
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

// cmpModel prefers the payload display name, else the resolved model segment.
func cmpModel(display, fallback string) string {
	if display != "" {
		return display
	}
	return fallback
}

// appendSep appends s after a separator when row already has cells.
func appendSep(row []twoSeg, s twoSeg) []twoSeg {
	if len(row) > 0 {
		row = append(row, twoSeg{key: "sep", text: twoSep})
	}
	return append(row, s)
}

// paintTwoRow sweeps the ramp across the row's runes; each cell gets its themed background.
func paintTwoRow(row []twoSeg, th map[string]map[string]string, stops []ansi.RGB, mode string) string {
	total := 0
	for _, s := range row {
		total += len([]rune(s.text))
	}
	var b strings.Builder
	i := 0
	for _, s := range row {
		if bg := th[s.key]["bg"]; bg != "" {
			b.WriteString(ansi.BgSeq(mode, ansi.Hex(bg)))
		}
		if s.dim || s.key == "sep" {
			b.WriteString("\x1b[2m")
		}
		for _, r := range s.text {
			rgb := ansi.GradAt(stops, float64(i)/float64(max(total-1, 1)))
			if s.key == "sep" {
				rgb = ansi.Hex(cmpModel(th["sep"]["fg"], "#CAD3F5"))
			} else if s.fg != "" {
				rgb = ansi.Hex(s.fg)
			}
			b.WriteString(ansi.FgSeq(mode, rgb, s.bold) + string(r))
			i++
		}
		b.WriteString(ansi.Reset)
	}
	return b.String()
}

// renderTwoline draws the two-row style: where on row 1, auth/context/tokens/7d on row 2.
func renderTwoline(v values, cfg Config, pal theme.Palettes, stops []ansi.RGB, mode string) string {
	th := pal.Powerline["minimal"]
	row1, row2 := twolineRows(v)
	for i := range row1 { // velocity keeps the add/del tones every style uses
		if k := row1[i].key; k == "add" || k == "del" {
			row1[i].fg = pal.Sem(k)
		}
	}
	out := paintTwoRow(row1, th, stops, mode)
	if head := promptHead(v, cfg, pal, stops, mode); head != "" { // the PS1 head leads, as the other styles draw it
		out = head + paintTwoRow([]twoSeg{{key: "sep", text: sepPath}}, th, stops, mode) + out
	}
	if len(row2) == 0 {
		return out
	}
	return out + "\n" + paintTwoRow(row2, th, stops, mode)
}
