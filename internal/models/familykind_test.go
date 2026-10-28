package models

import "testing"

func TestFamilyKindAndWireFlags(t *testing.T) {
	cases := []struct {
		id                                  string
		fam                                 Family
		respOnly, chatOnly, openAIReasoning bool
	}{
		{"claude-opus-5-5", FamilyClaude, false, false, false},
		{"databricks-claude-opus-5-5", FamilyClaude, false, false, false},
		{"system.ai.claude-opus-5-5", FamilyClaude, false, false, false},
		{"claude-opus-5.5", FamilyClaude, false, false, false},
		{"claude-opus-5-5[1m]", FamilyClaude, false, false, false},
		{"claude-3-7-sonnet", FamilyClaude, false, false, false},
		{"databricks-claude-3-7-sonnet", FamilyClaude, false, false, false},
		{"us.anthropic.claude-3-5-sonnet-20241022-v2:0", FamilyClaude, false, false, false},
		{"gpt-oss-120b", FamilyGPT, false, true, false},
		{"databricks-gpt-oss-120b", FamilyGPT, false, true, false},
		{"gpt-6-luna", FamilyGPT, false, false, true},
		{"databricks-gpt-6-1-sol", FamilyGPT, true, false, true},
		{"gpt-5.5", FamilyGPT, true, false, true},
		{"gemini-4-argon", FamilyGemini, false, false, false},
		{"totally-unknown-model", FamilyOther, false, false, false},
	}
	for _, c := range cases {
		if got := FamilyKind(c.id); got != c.fam {
			t.Errorf("FamilyKind(%q) = %q, want %q", c.id, got, c.fam)
		}
		if got := ResponsesOnly(c.id); got != c.respOnly {
			t.Errorf("ResponsesOnly(%q) = %v, want %v", c.id, got, c.respOnly)
		}
		if got := ChatOnly(c.id); got != c.chatOnly {
			t.Errorf("ChatOnly(%q) = %v, want %v", c.id, got, c.chatOnly)
		}
		if got := IsOpenAIReasoning(c.id); got != c.openAIReasoning {
			t.Errorf("IsOpenAIReasoning(%q) = %v, want %v", c.id, got, c.openAIReasoning)
		}
	}
}
