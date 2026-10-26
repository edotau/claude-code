// labels.go — an agent's mode column from agentPlans + agentPlanFormat (vault overlay/providers), else today's text.
package configure

import (
	"cmp"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

// labelMax is the mode column's room in an 80-column row (name 11 · label · check 12, plus gaps).
const labelMax = 48

// defaultPlanFormat renders lowercase ("anthropic subscription max 20x"); a user's agentPlanFormat renders as written.
const defaultPlanFormat = "{provider} {mode} {tier}"

// agentVendor is the provider a plan names when its entry gives none.
var agentVendor = map[string]string{"claude": "Anthropic", "codex": "OpenAI", "gemini": "Google", "copilot": "GitHub", "opencode": "opencode"}

// modeLabel renders the mode column from agentPlanFormat with the matching agentPlans entry (tier and provider
// default to the detected plan and the agent's vendor). No entry and no format keeps today's label.
func modeLabel(d *dialog, name string, st agentState) string {
	if d.reg == nil || (len(d.reg.AgentPlans) == 0 && d.reg.AgentPlanFormat == "") {
		return st.label()
	}
	plans, ok := d.reg.AgentPlans[name]
	if !ok {
		plans = d.reg.AgentPlans[displayLabel(name)] // "antigravity" names gemini too
	}
	id := rawPlanID(d, name, st)
	plan, matched := matchPlan(plans, id)
	if !matched && d.reg.AgentPlanFormat == "" {
		return st.label()
	}
	format, lower := d.reg.AgentPlanFormat, false
	if format == "" {
		format, lower = defaultPlanFormat, true
	}
	out := strings.NewReplacer("{provider}", cmp.Or(plan.Provider, agentVendor[name]), "{mode}", st.mode,
		"{tier}", cmp.Or(plan.Tier, st.tier), "{id}", id).Replace(format)
	out = strings.Join(strings.Fields(out), " ") // an empty placeholder leaves no double space
	if lower {
		out = strings.ToLower(out)
	}
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
		if name == "copilot" && d.reg != nil {
			model, _ := providers.CopilotModel(d.reg)
			return model
		}
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
