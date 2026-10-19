// labels.go — the user's own text for an agent's mode column (providers.json agentLabels), else the computed one.
package configure

import (
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/vendors"
)

// labelMax keeps a custom mode column short enough for its row to stay within 80 columns.
const labelMax = 40

// modeLabel is the agent's mode column: the most specific agentLabels match (exact agent:rawID, then the longest
// glob, then the bare agent) rendered with {mode} {tier} {id} {provider}; the computed label when none matches.
func modeLabel(d *dialog, name string, st agentState) string {
	if d.reg == nil || len(d.reg.AgentLabels) == 0 {
		return st.label()
	}
	id, provider := rawPlanID(d, name, st)
	tmpl, ok := matchLabel(d.reg.AgentLabels, name, id)
	if !ok && displayLabel(name) != name { // keys may use the shown name too ("antigravity" for gemini)
		tmpl, ok = matchLabel(d.reg.AgentLabels, displayLabel(name), id)
	}
	if !ok {
		return st.label()
	}
	out := strings.NewReplacer("{mode}", st.mode, "{tier}", st.tier, "{id}", id, "{provider}", provider).Replace(tmpl)
	if r := []rune(out); len(r) > labelMax {
		out = string(r[:labelMax-1]) + "…"
	}
	return out
}

// matchLabel picks labels' template for agent with raw id: "agent:id" exactly, then the longest matching
// glob key (either half may use *), then "agent" alone.
func matchLabel(labels map[string]string, agent, id string) (string, bool) {
	if t, ok := labels[agent+":"+id]; ok && id != "" {
		return t, true
	}
	best := ""
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range labels {
			if !yield(k) {
				return
			}
		}
	}) {
		a, i, ok := strings.Cut(k, ":")
		if ok && strings.Contains(k, "*") && glob(a, agent) && glob(i, id) && len(k) > len(best) {
			best = k
		}
	}
	if best != "" {
		return labels[best], true
	}
	t, ok := labels[agent]
	return t, ok
}

// glob matches s against a pattern whose only wildcard is * (any run, / included).
func glob(pattern, s string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}

// rawPlanID is what the agent's sign-in reports (claude rate tier, ChatGPT plan, copilot_plan); on an API key it is
// the account's opus model id. provider names the account the agent runs on.
func rawPlanID(d *dialog, name string, st agentState) (id, provider string) {
	p, claude := modelTarget(d, name)
	if p != nil {
		provider = p.Name
	}
	if st.mode != modeSubscription {
		if p != nil {
			id = currentModel(d, p, claude, models.Opus)
		}
		return id, provider
	}
	switch name {
	case "claude":
		id = claudeRateTier()
	case "codex":
		id = vendors.CodexPlan()
	case "copilot":
		id = copilotRawPlan()
	}
	return id, provider
}
