package configure

import "testing"

// maxRuneLen is the widest label (rune count) among rows a picker renders together.
func maxRuneLen(labels []string) int {
	w := 0
	for _, l := range labels {
		if n := len([]rune(l)); n > w {
			w = n
		}
	}
	return w
}

// rowRenderedWidth mirrors tui's layoutRows + render: a 2-char marker/space prefix, every label padded to
// the widest in its row set, a 2-space spacer, then the detail column.
func rowRenderedWidth(labels []string, detail string) int {
	return 2 + maxRuneLen(labels) + 2 + len([]rune(detail))
}

// TestMenuRowWidthsFitEightyColumns: every row configure shows — the top agent picker and each agent's own
// submenu — must render at 80 columns or fewer; past that it wraps on an ordinary terminal, which (per the
// tui package's own wrapped-line accounting) is exactly what desyncs a redraw's rewind count.
func TestMenuRowWidthsFitEightyColumns(t *testing.T) {
	reg := menuTestEnv(t)
	d := &dialog{reg: reg, out: discardWriter{}}

	rows, _ := menuRows(d)
	rows = append(rows, submitRow)
	var labels []string
	for _, r := range rows {
		labels = append(labels, r[0])
	}
	for _, r := range rows {
		if w := rowRenderedWidth(labels, r[1]); w > 80 {
			t.Errorf("top menu row %q = %d cols, want <=80: %q", r[0], w, r[1])
		}
	}

	for name, build := range agentMenus {
		m := build(d)
		var subLabels []string
		for _, r := range m.rows {
			subLabels = append(subLabels, r.label)
		}
		subLabels = append(subLabels, "Back")
		for _, r := range m.rows {
			if w := rowRenderedWidth(subLabels, r.desc); w > 80 {
				t.Errorf("%s submenu row %q = %d cols, want <=80: %q", name, r.label, w, r.desc)
			}
		}
	}
}
