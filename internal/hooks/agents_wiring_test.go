package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/agents"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// agentsRunners are the agent-dispatch Native entries and the agents runner each must be.
var agentsRunners = map[string]core.Runner{
	"dispatch-effort": agents.Effort, "dispatch-failure": agents.Release, "dispatch-workflow": agents.Release,
	"dispatch-stop": agents.Stop, "codex-agent-route": agents.CodexAgentRoute, "codex-routing-start": agents.CodexRoutingStart,
	"codex-routing-result": agents.CodexRoutingResult, "codex-subagent-start": agents.CodexSubagentStart,
}

// Every agent-dispatch Native entry is the intended agents runner.
func TestNativeAgentsEntriesAreAgentsRunners(t *testing.T) {
	for name, want := range agentsRunners {
		if reflect.ValueOf(Native[name]).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("%s: Native entry is not the agents runner", name)
		}
	}
}

// A background Agent's PostToolUse reaches no dispatch runner (dispatch-workflow matches Workflow only): the claim stays.
func TestAgentPostToolUseKeepsBackgroundClaim(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if inflight.SessionOwner() == 0 {
		t.Skip("session owner unavailable")
	}
	pre := `{"hook_event_name":"PreToolUse","tool_name":"Agent","tool_use_id":"toolu_bg","tool_input":{"prompt":"x","model":"sonnet"}}`
	post := `{"hook_event_name":"PostToolUse","tool_name":"Agent","tool_use_id":"toolu_bg","tool_response":{"status":"async_launched"}}`
	RunEvent("PreToolUse", false, strings.NewReader(pre), &bytes.Buffer{}, &bytes.Buffer{})
	RunEvent("PostToolUse", false, strings.NewReader(post), &bytes.Buffer{}, &bytes.Buffer{})
	if m, _ := filepath.Glob(filepath.Join(inflight.Dir(), inflight.Prefix+"*toolu_bg")); len(m) != 1 {
		t.Fatalf("claim markers after the Agent PostToolUse = %v, want the one claimed at PreToolUse", m)
	}
}

// `claude-code hook PreToolUse --codex-agent-route` (RunArgs) must emit the same wire shape the native runner renders.
func TestCodexAgentRouteThroughRunKeepsWireShape(t *testing.T) {
	root := vaulttest.Use(t)
	t.Setenv(settings.AgentEnv, core.AgentCodex)
	t.Setenv(settings.CodexProfileEnv, "probe")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"probe":{"preset":"databricks","host":"https://example.invalid","auth":{"type":"none"}}}},"routing":{"cells":{"codex":{"sonnet":{"model":"gpt-6-astra","effort":"high"}}}}}`)
	catalog := filepath.Join(root, "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"models":[{"slug":"databricks-gpt-6-astra","visibility":"list"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(settings.CodexCatalogEnv, catalog)
	body, _ := json.Marshal(map[string]any{"session_id": "native-session", "tool_use_id": "spawn-1", "tool_name": "collaborationspawn_agent",
		"model": "databricks-gpt-6-1-sol", "tool_input": map[string]any{"fork_turns": "none", "message": "add a --verbose flag", "task_name": "probe_child", "agent_type": "default"}})

	var directOut, directErr, runOut, runErr bytes.Buffer
	if code := runDirect("codex-agent-route", bytes.NewReader(body), &directOut, &directErr); code != core.ExitProceed {
		t.Fatalf("direct exit = %d", code)
	}
	if code := RunArgs("PreToolUse", []string{"--codex-agent-route"}, bytes.NewReader(body), &runOut, &runErr); code != core.ExitProceed {
		t.Fatalf("Run exit = %d (stderr %s)", code, &runErr)
	}
	if runOut.Len() == 0 || runOut.String() != directOut.String() {
		t.Fatalf("Run output %q != direct %q (want a non-empty routed allow)", &runOut, &directOut)
	}
	h, _ := hosttest.OneDoc(t, runOut.String())["hookSpecificOutput"].(map[string]any)
	if updated, _ := h["updatedInput"].(map[string]any); h["permissionDecision"] != "allow" || updated["model"] != "databricks-gpt-6-astra" {
		t.Fatalf("wire shape = %v", h)
	}
}
