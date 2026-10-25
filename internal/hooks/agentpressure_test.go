package hooks

import (
	"strings"
	"testing"
)

// withPressure forces the Anthropic reading for one test, restoring TestMain's no-reading stub after.
func withPressure(t *testing.T, pct float64) {
	t.Helper()
	old := claudePressure
	claudePressure = func() (float64, string, bool) { return pct, "5h", true }
	t.Cleanup(func() { claudePressure = old })
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

const pressureSpawn = `{"tool_name":"Agent","session_id":"s","tool_input":{"subagent_type":"%s","prompt":"refactor the parser in a.go and b.go"%s}}`

func spawn(kind, extra string) string {
	return strings.Replace(strings.Replace(pressureSpawn, "%s", kind, 1), "%s", extra, 1)
}

func TestPressureDownshiftsUnsetModel(t *testing.T) {
	withPressure(t, 95)
	doc := runAgentEffortFull(t, spawn("general-purpose", ""))
	if doc.HSO.UpdatedInput["model"] != "haiku" || doc.HSO.UpdatedInput["subagent_type"] != "general-purpose" {
		t.Fatalf("95%% must downshift sonnet→haiku and keep the agent type: %+v", doc.HSO.UpdatedInput)
	}
	if !strings.Contains(doc.HSO.AdditionalContext, "5h window at 95%") || !strings.Contains(doc.HSO.AdditionalContext, "--agent codex --model gpt-6.1-sol") {
		t.Errorf("context = %q, want the window, the percentage and the route pick", doc.HSO.AdditionalContext)
	}
}

func TestPressureOffloadsCodeWorker(t *testing.T) {
	withPressure(t, 98)
	got, _ := runAgentEffort(t, spawn("code-worker", ""))
	if got["subagent_type"] != "coworker" || got["model"] != "haiku" {
		t.Fatalf("98%% code-worker = %+v, want coworker at haiku", got)
	}
	if p, _ := got["prompt"].(string); !strings.HasPrefix(p, "Routed by agent-effort") || !strings.Contains(p, "--agent codex") ||
		!strings.Contains(p, "refactor the parser") {
		t.Errorf("prompt = %q, want the route prepended to the brief", p)
	}
	other, _ := runAgentEffort(t, spawn("Plan", ""))
	if other["subagent_type"] != "Plan" || other["model"] != "haiku" {
		t.Errorf("other types get the downshift and warning only: %+v", other)
	}
}

func TestPressureNeverTouchesExplicitModelOrOptOut(t *testing.T) {
	withPressure(t, 99)
	if got, _ := runAgentEffort(t, spawn("code-worker", `,"model":"opus"`)); got != nil && (got["model"] != "opus" || got["subagent_type"] != "code-worker") {
		t.Errorf("explicit model rewritten: %+v", got)
	}
	t.Setenv("HARNESS_ROUTE_SUBAGENTS", "0")
	if got, _ := runAgentEffort(t, spawn("code-worker", "")); got["subagent_type"] != "code-worker" || got["model"] != "sonnet" {
		t.Errorf("opt-out must leave only the classifier pin: %+v", got)
	}
}

func TestPressureBelowLowIsSilent(t *testing.T) {
	withPressure(t, 50)
	if got, _ := runAgentEffort(t, spawn("code-worker", "")); got["model"] != "sonnet" || got["subagent_type"] != "code-worker" {
		t.Errorf("50%% must not reroute: %+v", got)
	}
}
