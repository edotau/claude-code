package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/paths"
)

func readCodexAuditFixture(t *testing.T) ([]codexRouteRecord, []byte) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(paths.StateDir(), "codex", "routing-*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("audit files = %v, error %v", files, err)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var rows []codexRouteRecord
	for line := range bytes.SplitSeq(bytes.TrimSpace(body), []byte("\n")) {
		var row codexRouteRecord
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("audit mode = %o, want 600", info.Mode().Perm())
	}
	return rows, body
}

func runCodexAuditHandler(t *testing.T, handler Handler, payload map[string]any) {
	t.Helper()
	payload["session_id"] = "native-session"
	payload["prompt"] = "PROMPT-SECRET-SENTINEL"
	payload["tool_input"] = map[string]any{"message": "TASK-SECRET-SENTINEL", "task_name": "NAME-SECRET-SENTINEL"}
	var out, stderr bytes.Buffer
	if code := handler(bytes.NewReader(mustCodexJSON(t, payload)), &out, &stderr); code != ExitProceed {
		t.Fatalf("exit = %d", code)
	}
	if out.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("unexpected output %q / %q", &out, &stderr)
	}
}

func verifiedCodexRows(rows []codexRouteRecord) []codexRouteRecord {
	var out []codexRouteRecord
	for _, row := range rows {
		if row.Event == "verified" {
			out = append(out, row)
		}
	}
	return out
}

func TestCodexAuditCausalJoinBothArrivalOrders(t *testing.T) {
	for _, order := range []string{"result first", "child first"} {
		for _, model := range []string{codexRouteWireModel, "databricks-gpt-6-sol"} {
			t.Run(order+"/"+model, func(t *testing.T) {
				setupCodexRoute(t)
				if runCodexRoute(t, codexRoutePayload(map[string]any{"fork_turns": "none", "message": "add a --verbose flag"})) == nil {
					t.Fatal("fixture did not route")
				}
				result := func() {
					runCodexAuditHandler(t, CodexRoutingResult, map[string]any{"tool_name": "collaborationspawn_agent", "tool_use_id": "spawn-1", "tool_response": `{"agent_id":"child-uuid"}`})
				}
				child := func() {
					runCodexAuditHandler(t, CodexSubagentStart, map[string]any{"agent_id": "child-uuid", "model": model})
				}
				if order == "result first" {
					result()
				} else {
					child()
				}
				rows, _ := readCodexAuditFixture(t)
				if len(verifiedCodexRows(rows)) != 0 {
					t.Fatal("verified before the causal chain was complete")
				}
				if order == "result first" {
					child()
				} else {
					result()
				}
				rows, body := readCodexAuditFixture(t)
				verified := verifiedCodexRows(rows)
				if len(verified) != 1 {
					t.Fatalf("verified rows = %+v", verified)
				}
				got := verified[0]
				if got.ToolUseID != "spawn-1" || got.AgentID != "child-uuid" || got.Model != model || got.RequestedModel != codexRouteWireModel || got.Matches == nil || *got.Matches != (model == codexRouteWireModel) {
					t.Fatalf("causal verification = %+v", got)
				}
				result()
				child()
				rows, _ = readCodexAuditFixture(t)
				if len(verifiedCodexRows(rows)) != 1 {
					t.Fatal("duplicate events duplicated verification")
				}
				if strings.Contains(string(body), "SECRET-SENTINEL") {
					t.Fatal("audit leaked task or prompt contents")
				}
			})
		}
	}
}

func TestCodexAuditNativeResultWithoutAgentIDRemainsUnknown(t *testing.T) {
	setupCodexRoute(t)
	if runCodexRoute(t, codexRoutePayload(map[string]any{"fork_turns": "none", "message": "add a --verbose flag"})) == nil {
		t.Fatal("fixture did not route")
	}
	runCodexAuditHandler(t, CodexRoutingStart, map[string]any{"model": "databricks-gpt-6-1-sol"})
	runCodexAuditHandler(t, CodexRoutingResult, map[string]any{"tool_name": "collaborationspawn_agent", "tool_use_id": "spawn-1", "tool_response": `{"task_name":"/root/probe_child"}`})
	runCodexAuditHandler(t, CodexSubagentStart, map[string]any{"agent_id": "native-child-uuid", "model": codexRouteWireModel})
	rows, body := readCodexAuditFixture(t)
	if len(rows) != 4 || len(verifiedCodexRows(rows)) != 0 {
		t.Fatalf("native unmatched records = %+v", rows)
	}
	for _, row := range rows {
		if row.Matches != nil {
			t.Fatalf("uncorrelated row has verdict: %+v", row)
		}
		if row.SessionID != "native-session" || row.At == 0 {
			t.Fatalf("missing record metadata: %+v", row)
		}
	}
	for _, sentinel := range []string{"SECRET-SENTINEL", "/root/probe_child", "tool_input", "tool_response"} {
		if strings.Contains(string(body), sentinel) {
			t.Fatalf("audit leaked %q", sentinel)
		}
	}
}

func TestCodexAuditConcurrentSameModelChildrenUseIDs(t *testing.T) {
	setupCodexRoute(t)
	for _, id := range []string{"spawn-a", "spawn-b"} {
		payload := codexRoutePayload(map[string]any{"fork_turns": "none", "message": "add a --verbose flag"})
		payload["tool_use_id"] = id
		if runCodexRoute(t, payload) == nil {
			t.Fatal("fixture did not route")
		}
	}
	runCodexAuditHandler(t, CodexSubagentStart, map[string]any{"agent_id": "child-b", "model": codexRouteWireModel})
	runCodexAuditHandler(t, CodexSubagentStart, map[string]any{"agent_id": "child-a", "model": codexRouteWireModel})
	rows, _ := readCodexAuditFixture(t)
	if len(verifiedCodexRows(rows)) != 0 {
		t.Fatal("same model was used as a causal join")
	}
	runCodexAuditHandler(t, CodexRoutingResult, map[string]any{"tool_name": "spawn_agent", "tool_use_id": "spawn-a", "tool_response": map[string]any{"agent_id": "child-a"}})
	runCodexAuditHandler(t, CodexRoutingResult, map[string]any{"tool_name": "spawn_agent", "tool_use_id": "spawn-b", "tool_response": map[string]any{"agent_id": "child-b"}})
	rows, _ = readCodexAuditFixture(t)
	verified := verifiedCodexRows(rows)
	if len(verified) != 2 {
		t.Fatalf("verified rows = %+v", verified)
	}
	for _, row := range verified {
		if row.AgentID != "child-"+strings.TrimPrefix(row.ToolUseID, "spawn-") || row.Matches == nil || !*row.Matches {
			t.Fatalf("crossed concurrent child IDs: %+v", row)
		}
	}
}

func TestCodexAuditMissingCausalFieldsRemainUnknown(t *testing.T) {
	for _, missing := range []string{"tool ID", "agent ID", "child model"} {
		t.Run(missing, func(t *testing.T) {
			setupCodexRoute(t)
			payload := codexRoutePayload(map[string]any{"fork_turns": "none", "message": "add a --verbose flag"})
			result := map[string]any{"tool_name": "spawn_agent", "tool_use_id": "spawn-1", "tool_response": map[string]any{"agent_id": "child"}}
			child := map[string]any{"agent_id": "child", "model": codexRouteWireModel}
			switch missing {
			case "tool ID":
				payload["tool_use_id"] = ""
				result["tool_use_id"] = ""
			case "agent ID":
				result["tool_response"] = map[string]any{}
				child["agent_id"] = ""
			case "child model":
				delete(child, "model")
			}
			if runCodexRoute(t, payload) == nil {
				t.Fatal("fixture did not route")
			}
			runCodexAuditHandler(t, CodexRoutingResult, result)
			runCodexAuditHandler(t, CodexSubagentStart, child)
			rows, _ := readCodexAuditFixture(t)
			if len(verifiedCodexRows(rows)) != 0 {
				t.Fatalf("verified missing %s: %+v", missing, rows)
			}
		})
	}
}

func TestCodexAuditIgnoresOtherVendorsAndMissingSessions(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, vendor := range []string{VendorClaude, VendorCopilot, VendorCodex} {
		t.Setenv(hookspec.AgentEnv, vendor)
		for _, handler := range []Handler{CodexRoutingStart, CodexRoutingResult, CodexSubagentStart} {
			payload := map[string]any{"tool_name": "spawn_agent", "agent_id": "child", "model": codexRouteWireModel}
			if vendor != VendorCodex {
				payload["session_id"] = "session"
			}
			var out, stderr bytes.Buffer
			if handler(bytes.NewReader(mustCodexJSON(t, payload)), &out, &stderr) != ExitProceed {
				t.Fatal("audit blocked")
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(paths.StateDir(), "codex", "routing-*.jsonl"))
	if err != nil || len(files) != 0 {
		t.Fatalf("unexpected audit files = %v, error %v", files, err)
	}
}
