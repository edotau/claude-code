// Quota pressure on Claude subagents: near the Anthropic plan's limit the agent-effort hook downshifts an unset
// dispatch model one model (≥90%) or hands a code-worker/Explore task to a coworker on the routed agent (≥97%).
package hooks

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/usage"
)

// OffloadPct is the window share at which a code-worker or Explore dispatch moves to a coworker leaf.
const OffloadPct = 97.0

// envTestPct forces a reading for live checks; the subagents opt-out is providers.RouteSubagentsOn.
const envTestPct = "HARNESS_ROUTE_TEST_ANTHROPIC_PCT"

// offloadTypes are the agent types whose work a coworker leaf can carry through an agent CLI.
var offloadTypes = map[string]bool{"code-worker": true, "Explore": true}

// claudePressure is the forced test reading, else the cached Anthropic window; ok is false when neither exists.
var claudePressure = func() (float64, string, bool) {
	if v := os.Getenv(envTestPct); v != "" {
		pct, err := strconv.ParseFloat(v, 64)
		return pct, "test", err == nil
	}
	return usage.ClaudePressure(time.Now())
}

// routeHint names the agent pick for model slot ("codex gpt-6.1-sol"); "" when no agent passes the gates.
var routeHint = func(tier string) (agent, model string) {
	tb, err := router.Load()
	if err != nil {
		return "", ""
	}
	res := tb.Pick(context.Background(), tier, "", router.LoadState(), router.Opts{Fallback: router.FallbackVendors, Configured: agentpkg.Configured})
	if len(res.Candidates) == 0 {
		return "", ""
	}
	return res.Candidates[0].Agent, res.Candidates[0].Model
}

// billsSubscription is false only when the session provider is known and not passthrough (Databricks, an API key):
// the cached claude.ai windows then describe a plan this session does not spend.
func billsSubscription() bool {
	name := cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile))
	if u, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL")); err == nil && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
		if rest, ok := strings.CutPrefix(u.Path, "/p/"); ok {
			name, _, _ = strings.Cut(rest, "/")
		}
	}
	reg, err := providers.Load()
	if err != nil {
		return true
	}
	if name == "" {
		name = reg.Default
	}
	p := reg.Providers[name]
	return p == nil || p.Auth.Method() == providers.AuthCLI
}

// applyPressure rewrites updated under Anthropic quota pressure and returns the context note ("" = untouched).
// An explicit dispatch model is never changed; base is the model the dispatch will run (classifier or roster pin).
func applyPressure(in *Input, updated map[string]any, promptField, base string) (note, systemMessage string) {
	if on, _ := providers.RouteSubagentsOn(); !on {
		return "", ""
	}
	if os.Getenv(envTestPct) == "" && !billsSubscription() {
		return "", ""
	}
	pct, window, ok := claudePressure()
	if !ok || pct < router.LowPct {
		return "", ""
	}
	tier := base
	if tier != models.Opus && tier != models.Sonnet && tier != models.Haiku {
		tier = "sonnet"
	}
	agent, model := routeHint(tier)
	hint := "no agent passes the route gates"
	if agent != "" {
		hint = fmt.Sprintf("`claude-code ask --agent %s --model %s --tools`", agent, tier)
	}
	kind := in.nested("tool_input", "subagent_type")
	if pct >= OffloadPct && offloadTypes[kind] && agent != "" {
		updated["subagent_type"], updated["model"] = "coworker", "haiku"
		prompt, _ := updated[promptField].(string)
		updated[promptField] = fmt.Sprintf("Routed by agent-effort (Claude %s window at %.0f%%): do this task through %s, "+
			"then verify and report.\n\n%s", window, pct, hint, prompt)
		return fmt.Sprintf("agent-effort: Claude %s window at %.0f%% — %s dispatch moved to coworker (haiku) via %s.",
				window, pct, kind, hint),
			fmt.Sprintf("agent-effort: %.0f%% %s → coworker via %s %s (%s)", pct, window, agent, model, tier)
	}
	down := models.NextSlot(base)
	if down == "" && base == models.Haiku {
		down = base
	}
	if down != "" && down != base {
		updated["model"] = down
	}
	return fmt.Sprintf("agent-effort: Claude %s window at %.0f%% — model %s→%s; offload agent work with %s.",
			window, pct, cmpOr(base, "inherit"), cmpOr(down, base, "inherit"), hint),
		fmt.Sprintf("agent-effort: %.0f%% %s → %s", pct, window, cmpOr(down, base, "inherit"))
}

func cmpOr(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
