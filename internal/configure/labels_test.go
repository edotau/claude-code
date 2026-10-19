package configure

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestMatchPlanExactThenLongestGlob(t *testing.T) {
	plans := []providers.Plan{{ID: "*max*", Tier: "short"}, {ID: "*claude_max_20x", Tier: "long"}, {ID: "pro", Tier: "exact"}}
	cases := map[string]string{"pro": "exact", "default_claude_max_20x": "long", "max_5x": "short"}
	for id, want := range cases {
		if p, ok := matchPlan(plans, id); !ok || p.Tier != want {
			t.Errorf("matchPlan(%q) = %+v %v, want tier %q", id, p, ok, want)
		}
	}
	if _, ok := matchPlan(plans, "free"); ok {
		t.Error("an unmatched id must fall back to the computed label")
	}
	if _, ok := matchPlan([]providers.Plan{{ID: "*"}}, ""); !ok {
		t.Error(`"*" must match an agent that reports no plan id`)
	}
}

func TestModeLabelJoinsGivenFieldsAndFallsBack(t *testing.T) {
	reg := menuTestEnv(t)
	reg.AgentPlans = map[string][]providers.Plan{
		"antigravity": {{ID: "*", Tier: "Google AI Pro", Desc: "5h quota"}},
		"codex":       {{ID: "*", Provider: "ChatGPT"}},
	}
	d := &dialog{reg: reg, out: discardWriter{}}
	if got := modeLabel(d, "gemini", agentState{mode: modeSubscription}); got != "Google AI Pro · 5h quota" {
		t.Errorf("antigravity label = %q", got)
	}
	if got := modeLabel(d, "codex", agentState{mode: modeSubscription, tier: "Plus"}); got != "subscription (Plus) · ChatGPT" {
		t.Errorf("codex label keeps the detected tier when tier is omitted: %q", got)
	}
	if got := modeLabel(d, "copilot", agentState{mode: modeSubscription, tier: "Pro+"}); got != "subscription (Pro+)" {
		t.Errorf("unlisted agent = %q, want the computed label", got)
	}
}
