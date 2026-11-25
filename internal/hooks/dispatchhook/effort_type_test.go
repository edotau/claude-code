package dispatchhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// The allowed types are the agents/ files plus the built-ins: adding or removing a file changes the set.
func TestDispatchTypeGateFollowsAgentFiles(t *testing.T) {
	cfg := useAgentsDir(t, "claude", "worker", "Plan")
	blocked := func(st, brief string) string {
		raw, _ := json.Marshal(map[string]any{"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": st, "prompt": brief, "description": "x"}})
		r := agentEffortResult(context.Background(), core.ParseInput(bytes.NewReader(raw)))
		if r.Decision != core.Block || !strings.HasPrefix(r.Reason, "dispatch-effort: subagent_type") {
			return ""
		}
		return r.Reason
	}
	for _, st := range []string{"claude", "worker", "Plan", "Explore", "general-purpose", "web-fetch", "claude-code-guide", "statusline-setup", ""} {
		if r := blocked(st, "do it"); r != "" {
			t.Errorf("%q blocked: %s", st, r)
		}
	}
	const roster = "(Explore, Plan, claude, claude-code-guide, general-purpose, statusline-setup, web-fetch, worker)"
	for st, c := range map[string]struct{ brief, use string }{
		"copilot": {"do it", `use "general-purpose"`}, "not-a-type": {"read a page", `use "general-purpose"`},
		"claude-context-optimizer:context-analyzer": {"q", `use "general-purpose"`},
		"coworker:copilot":                          {"do it", `use "copilot"`},
		"coworker:opencode":                         {"do it", `use "opencode"`},
		"coworker":                                  {"Agent: codex\n\ndo it", `use "general-purpose"`},
	} {
		want := fmt.Sprintf("dispatch-effort: subagent_type %q is not on the roster %s; %s", st, roster, c.use)
		if r := blocked(st, c.brief); r != want {
			t.Errorf("%s: reason %q, want %q", st, r, want)
		}
	}
	writeAgentFile(t, cfg, "helper")
	if r := blocked("helper", "do it"); r != "" {
		t.Errorf("helper with its file added still blocked: %s", r)
	}
	if err := os.Remove(filepath.Join(cfg, "agents", "worker.md")); err != nil {
		t.Fatal(err)
	}
	if r := blocked("worker", "do it"); !strings.Contains(r, "is not on the roster") {
		t.Errorf("worker with its file removed = %q, want blocked", r)
	}
	if err := os.Remove(filepath.Join(cfg, "agents", "Plan.md")); err != nil {
		t.Fatal(err)
	}
	if r := blocked("Plan", "do it"); !strings.Contains(r, `use "claude" and state read-only`) {
		t.Errorf("Plan with its file removed = %q, want blocked with the claude redirect", r)
	}
}

// An unreadable or empty agents dir never blocks: the roster falls back to agentid plus the built-ins.
func TestDispatchTypeGateFallsBackToRoster(t *testing.T) {
	orig := listAgents
	t.Cleanup(func() { listAgents = orig })
	listAgents = func(string) ([]project.Source, error) { return nil, os.ErrPermission }
	if got, want := dispatchTypes(), []string{"Explore", "claude", "claude-code-guide", "codex", "copilot", "coworker", "gemini", "general-purpose", "opencode", "statusline-setup", "web-fetch"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fallback types = %v, want %v", got, want)
	}
	useAgentsDir(t) // no agents/ at all
	listAgents = orig
	if r := typeBlock("gemini"); r != nil {
		t.Errorf("gemini with no agents dir blocked: %s", r.Reason)
	}
}

// A refused type is blocked before the cap claim: no in-flight marker is written.
func TestDispatchTypeGateClaimsNoSlot(t *testing.T) {
	dispatchEnv(t)
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "claude")
	before := markerNames(t)
	r := pre(`{"tool_name":"Agent","tool_use_id":"gate1","tool_input":{"subagent_type":"not-a-type","prompt":"x","description":"x"}}`)
	if r.Decision != core.Block || len(markerNames(t)) != len(before) || hasMarker(t, "gate1") {
		t.Fatalf("not-a-type = %+v, markers %v -> %v; want a block and no new marker", r, before, markerNames(t))
	}
	if r := pre(`{"tool_name":"Agent","tool_use_id":"gate2","tool_input":{"subagent_type":"claude","model":"opus","prompt":"x","description":"x"}}`); r.Decision == core.Block || !hasMarker(t, "gate2") {
		t.Fatalf("claude = %+v, want a claimed slot (negative control)", r)
	}
}

// coworker is a real type: it runs as sent; the retired coworker:<agent> is denied with the agent's type, any mode.
func TestDispatchCoworkerTypeIsReal(t *testing.T) {
	dispatchEnv(t)
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "claude")
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "coworker")
	payload := func(mode, st string) core.Result {
		return pre(fmt.Sprintf(`{"tool_name":"Agent","permission_mode":%q,"tool_input":{"subagent_type":%q,"prompt":"do it","description":"do it"}}`, mode, st))
	}
	if r := payload("default", "coworker"); r.Decision == core.Block || r.UpdatedInput["subagent_type"] != "coworker" {
		t.Errorf("coworker = %+v, want it to run as coworker", r)
	}
	for _, mode := range []string{"default", "auto"} {
		if r := payload(mode, "coworker:codex"); r.Decision != core.Block || !strings.HasSuffix(r.Reason, `use "codex"`) {
			t.Errorf("%s coworker:codex = %+v, want a block naming codex", mode, r)
		}
	}
}

// A retired name retyped to coworker pins model and effort exactly as a direct coworker dispatch.
func TestDispatchStaleRetypeMatchesDirect(t *testing.T) {
	dispatchEnv(t)
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if err := os.MkdirAll(filepath.Join(cfg, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: coworker\nmodel: haiku\neffort: low\n---\n"
	if err := os.WriteFile(filepath.Join(cfg, "agents", "coworker.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(st string) core.Result {
		return pre(fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"prompt":"fix a typo","description":"fix a typo"}}`, st))
	}
	direct, retyped := run("coworker"), run("code-mode")
	if retyped.UpdatedInput["subagent_type"] != "coworker" {
		t.Fatalf("code-mode = %+v, want a retype to coworker", retyped)
	}
	for _, k := range []string{"model", "effort"} {
		if direct.UpdatedInput[k] != retyped.UpdatedInput[k] {
			t.Errorf("%s: retyped %v, direct %v, want equal (the agent file pins both)", k, retyped.UpdatedInput[k], direct.UpdatedInput[k])
		}
	}
	if retyped.UpdatedInput["model"] != nil || retyped.UpdatedInput["effort"] != nil {
		t.Errorf("retyped = %v, want no model or effort pin over the agent file", retyped.UpdatedInput)
	}
}

func TestDispatchCodemodeRetype(t *testing.T) {
	dispatchEnv(t)
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "claude")
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "coworker")
	for _, c := range []struct{ st, brief, want string }{
		{"coworker", "Agent: pi\\nCodemode: off\\n\\nbody", "claude"},
		{"claude", "Agent: pi\\nCodemode: on\\n\\nbody", "coworker"},
		{"coworker", "Agent: pi\\n\\nbody", "coworker"},
		{"pi", "Agent: pi\\n\\nbody", "coworker"},
		{"pi", "Agent: pi\\nCodemode: off\\n\\nbody", "claude"},
		{"code-mode", "body", "coworker"},
		{"claude", "Agent: pi\\nrun --codemode off\\n\\nbody", "coworker"}, // prose is not the Codemode: line
		{"claude", "Agent: code-mode\\n\\nbody", "coworker"},
		{"claude", "Agent: pi\\nCodemode: off\\n\\nbody", "claude"},
		{"claude", "Agent: codex\\n\\nbody", "claude"},
	} {
		r := pre(fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"prompt":"%s","description":"x"}}`, c.st, c.brief))
		got, _ := r.UpdatedInput["subagent_type"].(string)
		if r.Decision == core.Block || got != c.want {
			t.Errorf("%s %q = %+v, want type %s", c.st, c.brief, r, c.want)
		}
	}
}

func TestDispatchCoworkerPiSuggestsCoworker(t *testing.T) {
	if got := typeSuggestion("coworker:pi"); got != `use "coworker"` {
		t.Errorf("suggestion = %q, want coworker", got)
	}
}

func TestDispatchCodeModeOffKeepsPiAgentLine(t *testing.T) {
	dispatchEnv(t)
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "claude")
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "coworker")
	r := pre(`{"tool_name":"Agent","tool_input":{"subagent_type":"coworker","prompt":"Codemode: off\n\nbody","description":"x"}}`)
	p, _ := r.UpdatedInput["prompt"].(string)
	if got, _ := r.UpdatedInput["subagent_type"].(string); got != "claude" || !strings.HasPrefix(p, "Agent: pi\n") {
		t.Errorf("type %q prompt %q, want claude with an Agent: pi header line", got, p)
	}
}

func TestAskLineRoutePicksAgentLineAcrossContinuations(t *testing.T) {
	text := "claude-code agents ask --agent claude --profile x\nclaude-code agents ask --background \\\n  --agent pi --provider databricks --profile qa - < PATH\n"
	if prov, prof := askLineRoute(text, "pi"); prov != "databricks" || prof != "qa" {
		t.Errorf("route = %q %q, want databricks qa", prov, prof)
	}
}
