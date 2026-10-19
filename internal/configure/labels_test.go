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

func TestModeLabelFormatsDefaultsAndFallsBack(t *testing.T) {
	reg := menuTestEnv(t)
	reg.AgentPlans = map[string][]providers.Plan{
		"antigravity": {{ID: "*", Tier: "Google AI Pro"}},
		"codex":       {{ID: "*"}},
	}
	d := &dialog{reg: reg, out: discardWriter{}}
	if got := modeLabel(d, "gemini", agentState{mode: modeSubscription}); got != "google subscription google ai pro" {
		t.Errorf("antigravity label = %q", got)
	}
	if got := modeLabel(d, "codex", agentState{mode: modeSubscription, tier: "Plus"}); got != "openai subscription plus" {
		t.Errorf("codex label (detected tier, vendor default) = %q", got)
	}
	if got := modeLabel(d, "copilot", agentState{mode: modeSubscription, tier: "Pro+"}); got != "subscription (Pro+)" {
		t.Errorf("unlisted agent = %q, want today's label", got)
	}
	reg.AgentPlanFormat = "{tier} · {provider}"
	if got := modeLabel(d, "copilot", agentState{mode: modeSubscription, tier: "Pro+"}); got != "Pro+ · GitHub" {
		t.Errorf("a set format applies to every agent, verbatim: %q", got)
	}
}
