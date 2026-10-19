// labels.go — an agent's mode column from the user's agentPlans (providers.json), else the computed one.
package configure

import (
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

// labelMax is the mode column's room in an 80-column row (name 11 · label · check 12, plus gaps).
const labelMax = 48

// modeLabel builds the mode column from the agentPlans entry matching the plan the sign-in reports: the entry's
// tier (else today's mode text), then its desc and provider when set. No entry keeps today's label.
func modeLabel(d *dialog, name string, st agentState) string {
	if d.reg == nil || len(d.reg.AgentPlans) == 0 {
		return st.label()
	}
	plans, ok := d.reg.AgentPlans[name]
	if !ok {
		plans = d.reg.AgentPlans[displayLabel(name)] // "antigravity" names gemini too
	}
	plan, ok := matchPlan(plans, rawPlanID(d, name, st))
	if !ok {
		return st.label()
	}
	parts := []string{withTier(st.mode, st.tier)}
	if plan.Tier != "" && st.mode == modeSubscription {
		parts[0] = plan.Tier // the plan name already says it is a subscription
	}
	for _, f := range []string{plan.Desc, plan.Provider} {
		if f != "" {
			parts = append(parts, f)
		}
	}
	out := strings.Join(parts, " · ")
	if r := []rune(out); len(r) > labelMax {
		out = string(r[:labelMax-1]) + "…"
	}
	return out
}

// matchPlan picks the plan whose id equals the raw id, else the longest matching * glob.
func matchPlan(plans []providers.Plan, id string) (providers.Plan, bool) {
	best, found := providers.Plan{}, false
	for _, p := range plans {
		switch {
		case p.ID == id:
			return p, true
		case strings.Contains(p.ID, "*") && glob(p.ID, id) && (!found || len(p.ID) > len(best.ID)):
			best, found = p, true
		}
	}
	return best, found
}

// glob matches s against a pattern whose only wildcard is * (any run, / included).
func glob(pattern, s string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}

// rawPlanID is what the agent's sign-in reports (claude rate tier, ChatGPT plan, copilot_plan); on an API key it
// is the account's opus model id, so a pattern can match a provider's prefixed ids.
func rawPlanID(d *dialog, name string, st agentState) string {
	if st.mode != modeSubscription {
		if p, claude := modelTarget(d, name); p != nil {
			return currentModel(d, p, claude, models.Opus)
		}
		return ""
	}
	switch name {
	case "claude":
		return claudeRateTier()
	case "codex":
		return vendors.CodexPlan()
	case "copilot":
		return copilotRawPlan()
	}
	return ""
}
