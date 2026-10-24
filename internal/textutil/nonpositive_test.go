package textutil

import "testing"

// Non-positive widths must not panic.
func TestNonPositiveWidths(t *testing.T) {
	for _, n := range []int{0, -1} {
		if got := Truncate("héllo", n); got != "" {
			t.Errorf("Truncate(_, %d) = %q, want empty", n, got)
		}
		if got := TruncateRunes("héllo", n); got != "" {
			t.Errorf("TruncateRunes(_, %d) = %q, want empty", n, got)
		}
	}
}
