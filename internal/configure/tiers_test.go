package configure

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeClaudeJSON(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if body == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeTier(t *testing.T) {
	cases := []struct{ name, json, plan, want string }{
		{"user 20x", `{"oauthAccount":{"userRateLimitTier":"default_claude_max_20x"}}`, "max", "Max 20x"},
		{"org 5x", `{"oauthAccount":{"organizationRateLimitTier":"default_claude_max_5x"}}`, "max", "Max 5x"},
		{"user beats org", `{"oauthAccount":{"userRateLimitTier":"default_claude_max_5x","organizationRateLimitTier":"default_claude_max_20x"}}`, "max", "Max 5x"},
		{"no tier, Max plan", `{"oauthAccount":{"userRateLimitTier":"default"}}`, "max", "Max"},
		{"no tier, Pro plan in parens", `{}`, " (pro) ", "Pro"},
		{"no file", ``, "pro", "Pro"},
		{"malformed file", `{`, "pro", "Pro"},
		{"nothing known", ``, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeClaudeJSON(t, c.json)
			if got := claudeTier(c.plan); got != c.want {
				t.Errorf("claudeTier(%q) = %q, want %q", c.plan, got, c.want)
			}
		})
	}
}

func TestCopilotTier(t *testing.T) {
	orig := copilotTierCmd
	t.Cleanup(func() { copilotTierCmd = orig })
	cases := []struct {
		name, out string
		err       error
		want      string
	}{
		{"individual", `{"copilot_plan":"individual"}`, nil, "Pro"},
		{"individual_pro", `{"copilot_plan":"individual_pro"}`, nil, "Pro+"},
		{"free", `{"copilot_plan":"free"}`, nil, "Free"},
		{"business", `{"copilot_plan":"business"}`, nil, "Business"},
		{"enterprise", `{"copilot_plan":"enterprise"}`, nil, "Enterprise"},
		{"unknown passes through", `{"copilot_plan":"new_tier"}`, nil, "new_tier"},
		{"empty plan", `{"copilot_plan":""}`, nil, ""},
		{"bad json", `nope`, nil, ""},
		{"command error", ``, errors.New("gh: not found"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clear(copilotPlanCache)
			copilotTierCmd = func(context.Context) ([]byte, error) { return []byte(c.out), c.err }
			if got := copilotTier(); got != c.want {
				t.Errorf("copilotTier = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTitleAndWithTier(t *testing.T) {
	if title("") != "" || title("plus") != "Plus" {
		t.Errorf("title: %q %q", title(""), title("plus"))
	}
	if got := withTier("subscription", ""); got != "subscription" {
		t.Errorf("withTier no tier = %q", got)
	}
	if got := withTier("subscription", "Max 20x"); got != "subscription (Max 20x)" {
		t.Errorf("withTier = %q", got)
	}
}

func TestAgentStateLabelAndCheck(t *testing.T) {
	cases := []struct {
		s            agentState
		label, check string
	}{
		{agentState{modeSubscription, "Max 20x", true}, "subscription (Max 20x)", "verified"},
		{agentState{mode: modeSubscription}, "subscription", "not verified"},
		{agentState{mode: modeAPIKey, tier: "ignored", verified: true}, "API key", "verified"},
		{agentState{mode: modeNone}, "not set up", "not verified"},
	}
	for _, c := range cases {
		if c.s.label() != c.label || c.s.check() != c.check {
			t.Errorf("%+v: label=%q check=%q, want %q/%q", c.s, c.s.label(), c.s.check(), c.label, c.check)
		}
	}
}
