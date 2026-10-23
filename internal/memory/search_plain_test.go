package memory

import "testing"

// TestSearchHitPlainStripsMarkup: a preview drops heading/bullet marks and id markers but keeps every word.
func TestSearchHitPlainStripsMarkup(t *testing.T) {
	h := SearchHit{Block: "## 2026-10-23 — Routing stays local <!-- id:7e92d61b -->\n\n- Rejected the remote router.\n"}
	want := "2026-10-23 — Routing stays local Rejected the remote router."
	if got := h.Plain(); got != want {
		t.Errorf("Plain() = %q, want %q", got, want)
	}
}
