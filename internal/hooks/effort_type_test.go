package hooks

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
	cfg := useAgentsDir(t, "claude", "codex", "Plan")
	blocked := func(st, brief string) string {
		raw, _ := json.Marshal(map[string]any{"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": st, "prompt": brief, "description": "x"}})
		r := agentEffortResult(context.Background(), core.ParseInput(bytes.NewReader(raw)))
		if r.Decision != core.Block || !strings.HasPrefix(r.Reason, "dispatch-effort: subagent_type") {
			return ""
		}
		return r.Reason
	}
	for _, st := range []string{"claude", "codex", "Plan", "Explore", "general-purpose", "web-fetch", ""} {
		if r := blocked(st, "do it"); r != "" {
			t.Errorf("%q blocked: %s", st, r)
		}
	}
	const roster = "(Explore, Plan, claude, codex, general-purpose, web-fetch)"
	for st, c := range map[string]struct{ brief, use string }{
		"copilot": {"do it", `use "general-purpose"`}, "not-a-type": {"read a page", `use "general-purpose"`},
		"claude-code-guide": {"q", `use "general-purpose"`}, "statusline-setup": {"q", `use "general-purpose"`},
		"claude-context-optimizer:context-analyzer": {"q", `use "general-purpose"`},
		"coworker:copilot":                          {"do it", `use "copilot"`},
		"coworker:opencode":                         {"do it", `use "opencode"`},
		"coworker":                                  {"do it", "name one roster agent"},
	} {
		want := fmt.Sprintf("dispatch-effort: subagent_type %q is not on the roster %s; %s", st, roster, c.use)
		if r := blocked(st, c.brief); r != want {
			t.Errorf("%s: reason %q, want %q", st, r, want)
		}
	}
	writeAgentFile(t, cfg, "copilot")
	if r := blocked("copilot", "do it"); r != "" {
		t.Errorf("copilot with its file added still blocked: %s", r)
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
	if got, want := dispatchTypes(), []string{"Explore", "claude", "codex", "copilot", "gemini", "general-purpose", "opencode", "web-fetch"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("fallback types = %v, want %v", got, want)
	}
	useAgentsDir(t) // no agents/ at all
	listAgents = orig
	if r := typeBlock("gemini", ""); r != nil {
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

// A coworker type naming exactly one roster agent is rewritten; an ambiguous one stays denied.
func TestDispatchCoworkerTypeRewrite(t *testing.T) {
	dispatchEnv(t)
	writeAgentFile(t, os.Getenv("CLAUDE_CONFIG_DIR"), "copilot")
	payload := func(mode, st string) core.Result {
		return pre(fmt.Sprintf(`{"tool_name":"Agent","permission_mode":%q,"tool_input":{"subagent_type":%q,"prompt":"do it","description":"do it"}}`, mode, st))
	}
	r := payload("default", "coworker:copilot")
	if r.Decision == core.Block || r.UpdatedInput["subagent_type"] != "copilot" || !strings.Contains(r.SystemMessage, `"copilot"`) {
		t.Errorf("coworker:copilot = %+v, want subagent_type rewritten to copilot with a message", r)
	}
	if r := payload("default", "coworker"); r.Decision != core.Block {
		t.Errorf("ambiguous coworker = %+v, want a block", r)
	}
	if r := payload("auto", "coworker:copilot"); r.Decision == core.Block || r.UpdatedInput["subagent_type"] != "copilot" {
		t.Errorf("auto coworker:copilot = %+v, want the same rewrite as default mode", r)
	}
}

// A coworker type rewritten to its roster agent pins model and effort exactly as a direct dispatch of that agent.
func TestDispatchCoworkerRetypeMatchesDirect(t *testing.T) {
	dispatchEnv(t)
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if err := os.MkdirAll(filepath.Join(cfg, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: copilot\nmodel: sonnet\neffort: medium\n---\n"
	if err := os.WriteFile(filepath.Join(cfg, "agents", "copilot.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(st string) core.Result {
		return pre(fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"prompt":"fix a typo","description":"fix a typo"}}`, st))
	}
	direct, retyped := run("copilot"), run("coworker:copilot")
	for _, k := range []string{"model", "effort"} {
		if direct.UpdatedInput[k] != retyped.UpdatedInput[k] {
			t.Errorf("%s: retyped %v, direct %v, want equal (the agent file pins both)", k, retyped.UpdatedInput[k], direct.UpdatedInput[k])
		}
	}
	if retyped.UpdatedInput["model"] != nil || retyped.UpdatedInput["effort"] != nil {
		t.Errorf("retyped = %v, want no model or effort pin over the agent file", retyped.UpdatedInput)
	}
}
