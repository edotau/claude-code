// Quota pressure on Claude subagents: near the Anthropic plan's limit the agent-effort hook downshifts an unset
// dispatch model one model (≥90%) or hands a code-worker task to a coworker on the routed agent (≥97%).
package agents

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
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/usage"
)

// offloadPct is the window share at which a code-worker dispatch moves to a coworker leaf.
const offloadPct = 97.0

// envTestPct forces a reading for live checks; the subagents opt-out is providers.RouteSubagentsOn.
const envTestPct = "HARNESS_ROUTE_TEST_ANTHROPIC_PCT"

// offloadTypes are the write-capable agent types a coworker leaf can carry; read-only types (Explore) never escalate.
var offloadTypes = map[string]bool{"code-worker": true, "code-worker:claude": true}

// ClaudePressure is the forced test reading, else the cached Anthropic window; ok is false when neither exists.
var ClaudePressure = func() (float64, string, bool) {
	if v := os.Getenv(envTestPct); v != "" {
		pct, err := strconv.ParseFloat(v, 64)
		return pct, "test", err == nil
	}
	return usage.ClaudePressure(time.Now())
}

// RouteHint names the agent pick for model alias ("codex gpt-6.1-sol"); "" when no agent passes the gates.
var RouteHint = func(alias string) (agent, model string) {
	tb, err := router.Load()
	if err != nil {
		return "", ""
	}
	opts, err := agentpkg.RosterPick(router.PickOpts{Fallback: router.FallbackAgents})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-effort route hint: %v\n", err)
		return "", ""
	}
	res := tb.Pick(context.Background(), alias, "", router.LoadState(), opts)
	if len(res.Candidates) == 0 {
		return "", ""
	}
	return res.Candidates[0].Agent, res.Candidates[0].Model
}

// billsSubscription is false only when the session provider is known and not passthrough (Databricks, an API key):
// the cached claude.ai windows then describe a plan this session does not spend.
func billsSubscription() bool {
	name := cmp.Or(providers.SessionProfile(), providers.AgentRecord("claude").Profile)
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
func applyPressure(in *core.Input, updated map[string]any, promptField, base string) (note, systemMessage string) {
	pct, window, ok := pressureActive()
	if !ok {
		return "", ""
	}
	alias := pressureAlias(base)
	agent, model := RouteHint(alias)
	hint := ""
	if agent != "" {
		hint = fmt.Sprintf("`claude-code agents ask --agent %s --model %s --tools`", agent, alias)
	}
	kind := in.Nested("tool_input", "subagent_type")
	if pct >= offloadPct && offloadTypes[kind] && agent != "" {
		to := poolType(agent)
		offloadToCoworker(updated, promptField, window, hint, to, pct)
		return fmt.Sprintf("agent-effort: Claude %s window at %.0f%% — %s dispatch moved to %s (haiku) via %s.",
				window, pct, kind, to, hint),
			fmt.Sprintf("agent-effort: %.0f%% %s → %s via %s %s (%s)", pct, window, to, agent, model, alias)
	}
	down := downshiftModel(updated, base)
	offload := ""
	if hint != "" {
		offload = "; offload agent work with " + hint
	}
	return fmt.Sprintf("agent-effort: Claude %s window at %.0f%% — model %s→%s%s.",
			window, pct, cmp.Or(base, "inherit"), cmp.Or(down, base, "inherit"), offload),
		fmt.Sprintf("agent-effort: %.0f%% %s → %s", pct, window, cmp.Or(down, base, "inherit"))
}

// pressureAlias maps the dispatch's model onto the alias the offload hint is routed for.
func pressureAlias(base string) string {
	if base != models.Opus && base != models.Sonnet && base != models.Haiku {
		return "sonnet"
	}
	return base
}

// downshiftModel moves the dispatch one model alias down (haiku stays) and returns the alias chosen ("" = none).
func downshiftModel(updated map[string]any, base string) string {
	down := models.NextAlias(base)
	if down == "" && base == models.Haiku {
		down = base
	}
	if down != "" && down != base {
		updated["model"] = down
	}
	return down
}

// pressureActive reports the Claude quota pressure when subagent routing is on and the session bills a subscription.
func pressureActive() (pct float64, window string, ok bool) {
	if on, _ := providers.RouteSubagentsOn(); !on {
		return 0, "", false
	}
	if os.Getenv(envTestPct) == "" && !billsSubscription() {
		return 0, "", false
	}
	pct, window, ok = ClaudePressure()
	if !ok || pct < router.LowPct {
		return 0, "", false
	}
	return pct, window, true
}

// poolType is the plugin leaf pinned to agent (code-worker:codex, coworker:opencode, …), else the bare coworker.
func poolType(agent string) string {
	switch agent {
	case "codex", "gemini":
		return "code-worker:" + agent
	case "copilot", "opencode":
		return "coworker:" + agent
	}
	return "coworker"
}

// offloadToCoworker retargets the dispatch at a haiku leaf of type to and prefixes its prompt with the routing note.
func offloadToCoworker(updated map[string]any, promptField, window, hint, to string, pct float64) {
	updated["subagent_type"], updated["model"] = to, "haiku"
	prompt, _ := updated[promptField].(string)
	updated[promptField] = fmt.Sprintf("Routed by agent-effort (Claude %s window at %.0f%%): do this task through %s, "+
		"then verify and report.\n\n%s", window, pct, hint, prompt)
}
