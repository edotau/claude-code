package models

import "testing"

func TestCanonical(t *testing.T) {
	cases := []struct {
		in   string
		want string
		oneM bool
	}{
		{"opus-5-5[1m]", "claude-opus-5-5", true},
		{"databricks-claude-opus-5-5", "claude-opus-5-5", false},
		{"system.ai.claude-opus-5", "claude-opus-5", false},
		{"system.ai-claude-sonnet-5", "claude-sonnet-5", false},
		{"my-endpoint", "my-endpoint", false},
		{"gpt-5", "gpt-5", false},
		{"databricks-my-endpoint", "databricks-my-endpoint", false},
		{"a/b", "a/b", false},
		{"  claude-opus-5[1m]  ", "claude-opus-5", true},
		{"  opus  ", "opus", false},
		{"system.ai.opus", "opus", false},
		{"claude-opus-5-5", "claude-opus-5-5", false},
	}
	for _, c := range cases {
		got, oneM := Canonical(c.in)
		if got != c.want || oneM != c.oneM {
			t.Errorf("Canonical(%q) = %q,%v; want %q,%v", c.in, got, oneM, c.want, c.oneM)
		}
		if again, _ := Canonical(got); again != got {
			t.Errorf("Canonical not idempotent on %q: %q", got, again)
		}
	}
}
