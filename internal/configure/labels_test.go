package configure

import "testing"

func TestMatchLabelPrecedence(t *testing.T) {
	labels := map[string]string{
		"claude":                        "bare",
		"claude:*max*":                  "short glob",
		"claude:*claude_max_20x":        "longer glob",
		"claude:default_claude_max_20x": "exact",
		"*:*claude-opus*":               "any agent opus",
	}
	cases := []struct{ agent, id, want string }{
		{"claude", "default_claude_max_20x", "exact"},
		{"claude", "team_claude_max_20x", "longer glob"},
		{"claude", "max_5x", "short glob"},
		{"claude", "pro", "bare"},
		{"codex", "system.ai.claude-opus-5", "any agent opus"},
	}
	for _, c := range cases {
		if got, _ := matchLabel(labels, c.agent, c.id); got != c.want {
			t.Errorf("matchLabel(%s, %s) = %q, want %q", c.agent, c.id, got, c.want)
		}
	}
	if _, ok := matchLabel(labels, "copilot", "individual_pro"); ok {
		t.Error("an unlisted agent must fall back to the computed label")
	}
}

func TestModeLabelPlaceholdersAndCap(t *testing.T) {
	reg := menuTestEnv(t)
	reg.AgentLabels = map[string]string{"codex": "{mode} ({tier}) · ChatGPT", "copilot": string(make([]rune, 60))}
	d := &dialog{reg: reg, out: discardWriter{}}
	if got := modeLabel(d, "codex", agentState{mode: modeSubscription, tier: "Plus"}); got != "subscription (Plus) · ChatGPT" {
		t.Errorf("codex label = %q", got)
	}
	if got := []rune(modeLabel(d, "copilot", agentState{mode: modeAPIKey})); len(got) != labelMax {
		t.Errorf("long label is %d runes, want capped at %d", len(got), labelMax)
	}
	if got := modeLabel(d, "claude", agentState{mode: modeNone}); got != "not set up" {
		t.Errorf("unlisted agent label = %q, want the computed one", got)
	}
}
