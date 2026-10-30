package hooks

import (
	"strings"
	"testing"
)

func TestByokHintPredicate(t *testing.T) {
	old := byokPoolSize
	byokPoolSize = func() int { return 6 }
	t.Cleanup(func() { byokPoolSize = old })
	cases := []struct {
		name, st, task string
		want           bool
	}{
		{"coworker", "coworker", "do a thing", true},
		{"agent opencode", "general-purpose", "Agent: opencode\nedit x", true},
		{"agent copilot", "general-purpose", "Agent: copilot\nrun tests", true},
		{"explicit model", "coworker", "run with --model gpt-6 now", false},
		{"other agent", "general-purpose", "Agent: codex\nreview", false},
		{"no agent line", "Explore", "look around", false},
	}
	for _, c := range cases {
		got := byokHint(c.st, c.task)
		if (got != "") != c.want {
			t.Errorf("%s: hint = %q, want hint=%v", c.name, got, c.want)
		}
		if got != "" && !strings.Contains(got, "(6 models)") {
			t.Errorf("%s: hint %q lacks the pool size", c.name, got)
		}
	}
}
