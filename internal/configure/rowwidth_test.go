package configure

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/agent/vendors"
)

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

// TestHomeExtraRowsFitEightyColumns renders the tools/settings rows with worst-case state text.
func TestHomeExtraRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	rows := []homeRow{
		{"tailscale", "not connected", "a-long-hostname-01 · no admin key", nil},
		{"web search", "no key", "mcp: claude,codex", nil},
		{"browsers", "default chrome", "12 installed", nil},
	}
	rows = append(rows, settingsRows(&dialog{})...)
	laid, _ := rowsOf(rows)
	for _, r := range laid {
		if w := 2 + len([]rune("environment")) + 2 + len([]rune(r[1])); w > 80 {
			t.Errorf("%s row is %d columns: %q", r[0], w, r[1])
		}
	}
}

// TestSettingRowFitsEightyColumns: the widest "claude code" page row — the longest catalog key, its
// longest enum value or "false", plus the worst-case source suffix " · live differs" — stays <=80 cols.
func TestSettingRowFitsEightyColumns(t *testing.T) {
	var labels []string
	for _, f := range settings.Catalog {
		labels = append(labels, f.Key)
	}
	for _, f := range settings.Catalog {
		longest := "false"
		for _, v := range f.Values {
			if len(v) > len(longest) {
				longest = v
			}
		}
		tmpl := map[string]json.RawMessage{f.Key: json.RawMessage(`"` + longest + `"`)}
		ov := map[string]json.RawMessage{}
		live := map[string]json.RawMessage{f.Key: json.RawMessage(`"` + longest + "-differs" + `"`)}
		row := settingRow(f, tmpl, ov, live)
		if w := rowRenderedWidth(labels, row[1]); w > 80 {
			t.Errorf("settingRow(%s) = %d cols, want <=80: %q", f.Key, w, row[1])
		}
	}
}

// TestRoutingRowFitsEightyColumns: the widest "routing" page row — the longest field key, its longest
// value, plus the "default"/"vault" source suffix — stays <=80 cols.
func TestRoutingRowFitsEightyColumns(t *testing.T) {
	var labels []string
	for _, m := range vendors.Modules() {
		for _, f := range m.Fields() {
			labels = append(labels, f.Key)
		}
	}
	worst := providers.Routing{
		Weights: map[string]float64{"price": 0.123456789, "headroom": 0.876543211},
		Opencode: providers.OpencodeRouting{
			Ladder:          &providers.RouteOpencodeSubIDs,
			WorkRepoMarkers: &[]string{"gitlab.example.com", "github.example.net"},
			FreeTiers:       &[]string{"sonnet", "haiku"},
			FreeFirstOutput: "90s",
		},
	}
	for _, m := range vendors.Modules() {
		for _, f := range m.Fields() {
			detail := f.Get(worst) + "  " + routingSource(f, worst)
			if w := rowRenderedWidth(labels, detail); w > 80 {
				t.Errorf("routing field %s = %d cols, want <=80: %q", f.Key, w, detail)
			}
		}
	}
}

// TestTierRowsFitEightyColumns: tier · use · a long model id stays within 80 columns.
func TestTierRowsFitEightyColumns(t *testing.T) {
	for slot, use := range tierUse {
		row := fmt.Sprintf("  %-6s  %s", slot, padTo(use, 42)+"anthropic/claude-sonnet-5-5") // as tui.layoutRows lays it
		if w := len([]rune(row)); w > 80 {
			t.Errorf("%s tier row is %d columns: %q", slot, w, row)
		}
	}
}

// TestDatabricksRowsFitEightyColumns: the home row at its worst state text and every page/env/apply row stay <=80 cols.
func TestDatabricksRowsFitEightyColumns(t *testing.T) {
	menuTestEnv(t)
	rows := []homeRow{{"databricks", "prod · oauth-m2m", "token expires in 119m", nil}, {"databricks", "prod · oauth-m2m", "token expires in 23h59m", nil}}
	rows = append(rows, settingsRows(&dialog{})...)
	laid, _ := rowsOf(rows)
	for _, r := range laid {
		if w := 2 + len([]rune("environment")) + 2 + len([]rune(r[1])); w > 80 {
			t.Errorf("%s row is %d columns: %q", r[0], w, r[1])
		}
	}
	d := dbxEnv(t)
	if row := databricksRow(d); 2+len([]rune("environment"))+2+len([]rune(row.state))+2+len([]rune(row.check)) > 80 {
		t.Errorf("live databricks row too wide: %+v", row)
	}
	f := &dbxFake{}
	installDbxFake(t, f)
	f.picks = []int{1, 4, 0, -1, -1}
	runDatabricksPage(d)
	for i, set := range f.rows {
		var labels []string
		for _, r := range set {
			labels = append(labels, r[0])
		}
		for _, r := range set {
			if w := rowRenderedWidth(labels, r[1]); w > 80 {
				t.Errorf("%s row %q = %d cols: %q", f.titles[i], r[0], w, r[1])
			}
		}
	}
	worst := "prod"
	if w := rowRenderedWidth([]string{"dev", "qa", worst}, "oauth-m2m  token expires in 23h59m"); w > 80 {
		t.Errorf("env row worst case = %d cols", w)
	}
}
