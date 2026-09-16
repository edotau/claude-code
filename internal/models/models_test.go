package models

import "testing"

func TestParseAndKnobs(t *testing.T) {
	cases := []struct {
		id       string
		family   string
		oneM     string
		thinking string
	}{
		{"claude-opus-5", "opus", "claude-opus-5[1m]", ""},
		{"claude-sonnet-4-6", "sonnet", "claude-sonnet-4-6[1m]", "31999"},
		{"claude-haiku-4-5-20251001", "haiku", "claude-haiku-4-5-20251001", "31999"},
		{"claude-sonnet-4-20250514", "sonnet", "claude-sonnet-4-20250514", "31999"},
		{"anthropic/claude-opus-4-8", "opus", "anthropic/claude-opus-4-8[1m]", ""},
		{"gpt-5", "", "gpt-5", ""},
	}
	for _, c := range cases {
		if got := FamilyOf(c.id); got != c.family {
			t.Errorf("FamilyOf(%q)=%q want %q", c.id, got, c.family)
		}
		if got := MaybeAdd1M(c.id); got != c.oneM {
			t.Errorf("MaybeAdd1M(%q)=%q want %q", c.id, got, c.oneM)
		}
		if got, _ := ThinkingKnobs(c.id); got != c.thinking {
			t.Errorf("ThinkingKnobs(%q)=%q want %q", c.id, got, c.thinking)
		}
	}
	if _, maj, min, _ := ParseClaudeID("claude-haiku-4-5-20251001"); maj != 4 || min != 5 {
		t.Errorf("dated id parsed as %d.%d", maj, min)
	}
	if ContextWindow("x[1m]") != ContextWindow1M || Strip1M("x[1m]") != "x" {
		t.Error("[1m] handling")
	}
}
