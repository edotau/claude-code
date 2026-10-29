package models

import "testing"

func TestTierModel(t *testing.T) {
	for tier, want := range map[string]string{
		EffortLow: Haiku, EffortMedium: Sonnet, EffortHigh: Sonnet, "": "", "bogus": "", "LOW": "",
	} {
		if got := TierModel(tier); got != want {
			t.Errorf("TierModel(%q) = %q, want %q", tier, got, want)
		}
	}
}
