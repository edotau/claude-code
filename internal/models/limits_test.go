package models

import "testing"

func TestLimitsResolvesSpellings(t *testing.T) {
	for _, id := range []string{"claude-sonnet-5-5", "databricks-claude-sonnet-5-5", "system.ai.claude-sonnet-5-5", "claude-sonnet-5-5[1m]"} {
		if l, ok := Limits(id); !ok || l.Context != 1_000_000 || l.Output != 128_000 {
			t.Errorf("Limits(%q) = %+v, %v", id, l, ok)
		}
	}
	for _, id := range []string{"gpt-5", "not-a-model", ""} {
		if l, ok := Limits(id); ok {
			t.Errorf("Limits(%q) = %+v, want unknown", id, l)
		}
	}
}
