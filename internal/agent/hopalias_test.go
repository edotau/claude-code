package agent

import "testing"

func TestHopAliasTierLineBeatsClassifier(t *testing.T) {
	sonnet := func(string) string { return "sonnet" }
	if got := HopAlias("Tier: deep\nfix it", sonnet); got != "opus" {
		t.Errorf("deep brief hop alias = %q, want opus", got)
	}
	if got := HopAlias("", sonnet); got != "sonnet" {
		t.Errorf("empty brief = %q, want the classifier's", got)
	}
}
