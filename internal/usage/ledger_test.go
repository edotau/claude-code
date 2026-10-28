package usage

import "testing"

// An entry with no provider labels as the profile alone, never "·profile".
func TestRowLabelEmptyProvider(t *testing.T) {
	if got := rowLabel("", "prod"); got != "prod" {
		t.Errorf("rowLabel(\"\", prod) = %q, want prod", got)
	}
}
