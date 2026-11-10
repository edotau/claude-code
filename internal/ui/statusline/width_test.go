package statusline

import "testing"

func TestClipCountsWideCells(t *testing.T) {
	if got := visibleWidth("界界"); got != 4 {
		t.Errorf("wide text occupies %d columns, want 4", got)
	}
	if got := plain(clip("界界", 2)); got != "…" {
		t.Fatalf("clip wide text to 2 columns = %q, want ellipsis only", got)
	}
}
