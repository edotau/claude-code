// Quota pressure on Claude subagents: near the Anthropic plan's limit the agent-effort hook downshifts an unset
// dispatch model one tier (≥90%) or hands a code-worker/Explore task to a coworker on the routed vendor (≥97%).
package hooks

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/usage"
)

// OffloadPct is the window share at which a code-worker or Explore dispatch moves to a coworker leaf.
const OffloadPct = 97.0

// Env knobs: the opt-out (env or vault pin) and a forced reading for live checks.
const (
	envRouteSubagents = "HARNESS_ROUTE_SUBAGENTS"
	envTestPct        = "HARNESS_ROUTE_TEST_ANTHROPIC_PCT"
)

// offloadTypes are the agent types whose work a coworker leaf can carry through a vendor CLI.
var offloadTypes = map[string]bool{"code-worker": true, "Explore": true}

// downshift is one tier cheaper; haiku stays haiku.
var downshift = map[string]string{"opus": "sonnet", "sonnet": "haiku", "haiku": "haiku"}

// claudePressure is the forced test reading, else the cached Anthropic window; ok is false when neither exists.
var claudePressure = func() (float64, string, bool) {
	if v := os.Getenv(envTestPct); v != "" {
		pct, err := strconv.ParseFloat(v, 64)
		return pct, "test", err == nil
	}
	return usage.ClaudePressure(time.Now())
}

// routeHint names the vendor pick for tier ("codex gpt-6.1-sol"); "" when no vendor passes the gates.
var routeHint = func(tier string) (runner, model string) {
	tb, err := route.Load()
	if err != nil {
		return "", ""
	}
	res := tb.Pick(context.Background(), tier, "", route.LoadState(), route.Opts{Fallback: route.FallbackVendors})
	if len(res.Candidates) == 0 {
		return "", ""
	}
	return res.Candidates[0].Runner, res.Candidates[0].Model
}

// applyPressure rewrites updated under Anthropic quota pressure and returns the context note ("" = untouched).
// An explicit dispatch model is never changed; base is the model the dispatch will run (classifier or roster pin).
func applyPressure(in *Input, updated map[string]any, promptField, base string) (note, systemMessage string) {
	if strings.TrimSpace(providers.Pin(envRouteSubagents)) == "0" {
		return "", ""
	}
	pct, window, ok := claudePressure()
	if !ok || pct < route.LowPct {
		return "", ""
	}
	tier := base
	if _, known := downshift[tier]; !known {
		tier = "sonnet"
	}
	runner, model := routeHint(tier)
	hint := "no vendor passes the route gates"
	if runner != "" {
		hint = fmt.Sprintf("`claude-code ask --agent %s --model %s --tools`", runner, model)
	}
	kind := in.nested("tool_input", "subagent_type")
	rec := map[string]any{"decision_id": randomHex(8), "session_id": in.SessionID, "tool_use_id": in.nested("tool_use_id"),
		"subagent_type": kind, "pressure_pct": pct, "window": window, "route": strings.TrimSpace(runner + " " + model),
		"at": time.Now().Unix()}
	if pct >= OffloadPct && offloadTypes[kind] && runner != "" {
		updated["subagent_type"], updated["model"] = "coworker", "haiku"
		prompt, _ := updated[promptField].(string)
		updated[promptField] = fmt.Sprintf("Routed by agent-effort (Claude %s window at %.0f%%): do this task through %s, "+
			"then verify and report.\n\n%s", window, pct, hint, prompt)
		rec["decision"], rec["model"] = "offload", "haiku"
		appendDecision(rec)
		return fmt.Sprintf("agent-effort: Claude %s window at %.0f%% — %s dispatch moved to coworker (haiku) via %s.",
				window, pct, kind, hint),
			fmt.Sprintf("agent-effort: %.0f%% %s → coworker via %s %s", pct, window, runner, model)
	}
	down := downshift[base]
	if down != "" && down != base {
		updated["model"] = down
	}
	rec["decision"], rec["model"] = "downshift", cmpOr(down, base)
	appendDecision(rec)
	return fmt.Sprintf("agent-effort: Claude %s window at %.0f%% — model %s→%s; offload vendor work with %s.",
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
