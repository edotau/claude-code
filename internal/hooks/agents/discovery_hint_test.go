package agents

import (
	"strings"
	"testing"
)

func TestDiscoveryHintPredicate(t *testing.T) {
	old := discoveryPoolSize
	discoveryPoolSize = func() int { return 6 }
	t.Cleanup(func() { discoveryPoolSize = old })
	cases := []struct {
		name, st, task string
		want           bool
	}{
		{"copilot", "copilot", "do a thing", true},
		{"opencode", "opencode", "do a thing", true},
		{"codex", "codex", "do a thing", false},
		{"agent opencode", "general-purpose", "Agent: opencode\nedit x", true},
		{"agent copilot", "general-purpose", "Agent: copilot\nrun tests", true},
		{"explicit model", "copilot", "run with --model gpt-6 now", false},
		{"other agent", "general-purpose", "Agent: codex\nreview", false},
		{"no agent line", "Explore", "look around", false},
	}
	for _, c := range cases {
		got := discoveryHint(c.st, c.task)
		if (got != "") != c.want {
			t.Errorf("%s: hint = %q, want hint=%v", c.name, got, c.want)
		}
		if got != "" && !strings.Contains(got, "(6 models)") {
			t.Errorf("%s: hint %q lacks the pool size", c.name, got)
		}
	}
}
