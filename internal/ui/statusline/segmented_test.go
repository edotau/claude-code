package statusline

import "testing"

// Given a wide-rune value, layoutDashboard pads so header and value rows have equal display width.
func TestLayoutDashboardWideValueAligns(t *testing.T) {
	built := []builtCol{
		{"branch", "branch", runeCells("日本語", "g")},
		{"context", "context", runeCells("5%", "g")},
	}
	hdr, val := layoutDashboard(built, 2, 20)
	if hw, vw := cellsWidth(hdr), cellsWidth(val); hw != vw || vw != 20 {
		t.Errorf("display widths hdr %d val %d, want both 20", hw, vw)
	}
}
