package models

import "testing"

func TestAgentType(t *testing.T) {
	cases := []struct {
		route, model string
		code         bool
		want         Type
	}{
		{"", "claude-opus-5-5", true, TypePi},
		{"github", "claude-opus-5-5", false, TypeCopilot},
		{"databricks", "claude-opus-5-5", false, TypeClaude},
		{"", "databricks-claude-sonnet-5-5", false, TypeClaude},
		{"", "claude-sonnet-4-5-20250929", false, TypeClaude},
		{"", "claude-opus-4-8[1m]", false, TypeClaude},
		{"", "claude-fable-5-2", false, TypeClaude},
		{"", "databricks-gpt-6-1-sol", false, TypeCodex},
		{"", "gpt-5-4-mini", false, TypeCodex},
		{"", "gemini-4-argon-high", false, TypeGemini},
		{"", "databricks-kimi-k3", false, "kimi"},
		{"", "grok-4-7", false, "grok"},
		{"", "qwen3-next-80b-a3b-instruct", false, "qwen"},
		{"", "glm-5-3-flash", false, "glm"},
		{"", "databricks-gpt-oss-120b", false, "gpt-oss"},
		{"", "databricks-meta-llama-3-3-70b-instruct", false, "llama"},
		{"", "gemma-3-12b", false, "gemma"},
		{"", "", false, ""},
		{"", "4-7", false, ""},
		{"", "", true, TypePi},
	}
	for _, c := range cases {
		if got := AgentType(c.route, c.model, c.code); got != c.want {
			t.Errorf("AgentType(%q, %q, %v) = %q, want %q", c.route, c.model, c.code, got, c.want)
		}
	}
}
