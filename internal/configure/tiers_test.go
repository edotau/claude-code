package configure

import "testing"

func TestWithTier(t *testing.T) {
	if got := WithTier("subscription", ""); got != "subscription" {
		t.Errorf("withTier no tier = %q", got)
	}
	if got := WithTier("subscription", "Max 20x"); got != "subscription (Max 20x)" {
		t.Errorf("withTier = %q", got)
	}
}
