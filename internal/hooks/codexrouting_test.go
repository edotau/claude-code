package hooks

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const codexRouteWireModel = "databricks-gpt-6-astra"

func setupCodexRoute(t *testing.T) string {
	t.Helper()
	root := vaulttest.Use(t)
	t.Setenv(hookspec.AgentEnv, VendorCodex)
	t.Setenv(hookspec.CodexProfileEnv, "probe")
	t.Setenv(providers.PinCodexModel, "")
	t.Setenv(providers.PinCodexEffort, "")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"probe":{"preset":"databricks","host":"https://example.invalid","auth":{"type":"none"}}}},"routing":{"cells":{"codex":{"sonnet":{"model":"gpt-6-astra","effort":"high"}}}}}`)
	catalog := filepath.Join(root, "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"models":[{"slug":"`+codexRouteWireModel+`","visibility":"list"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hookspec.CodexCatalogEnv, catalog)
	return catalog
}

func codexRoutePayload(args map[string]any) map[string]any {
	return map[string]any{"session_id": "native-session", "tool_use_id": "spawn-1", "tool_name": "collaborationspawn_agent",
		"model": "databricks-gpt-6-1-sol", "tool_input": args}
}

func runCodexRoute(t *testing.T, payload any) map[string]any {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := CodexAgentRoute(bytes.NewReader(body), &out, &stderr); code != ExitProceed {
		t.Fatalf("exit = %d, want proceed", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected diagnostic: %s", &stderr)
	}
	if out.Len() == 0 {
		return nil
	}
	var doc struct {
		Output struct {
			Event    string         `json:"hookEventName"`
			Decision string         `json:"permissionDecision"`
			Input    map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Output.Event != "PreToolUse" || doc.Output.Decision != "allow" {
		t.Fatalf("unexpected hook envelope: %s", &out)
	}
	return doc.Output.Input
}

func TestCodexAgentRouteFreshSpawnPreservesArgumentsAndWireModel(t *testing.T) {
	for _, fork := range []map[string]any{{"fork_turns": "none"}, {"fork_context": false}} {
		t.Run(strings.TrimSpace(string(mustCodexJSON(t, fork))), func(t *testing.T) {
			setupCodexRoute(t)
			args := map[string]any{"message": "add a --verbose flag to the token command", "task_name": "probe_child",
				"items": []any{map[string]any{"type": "text", "text": "preserve me"}}, "agent_type": "default"}
			maps.Copy(args, fork)
			got := runCodexRoute(t, codexRoutePayload(args))
			want := maps.Clone(args)
			want["model"], want["reasoning_effort"] = codexRouteWireModel, "high"
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("updated arguments = %#v, want %#v", got, want)
			}
			rows, _ := readCodexAuditFixture(t)
			if len(rows) != 1 || rows[0].RequestedModel != codexRouteWireModel || rows[0].ToolUseID != "spawn-1" {
				t.Fatalf("decision audit = %+v", rows)
			}
		})
	}
}

func TestCodexAgentRouteSkipsIneligibleSpawns(t *testing.T) {
	cases := []struct {
		name string
		edit func(*testing.T, map[string]any, map[string]any, string)
	}{
		{"implicit fork", func(_ *testing.T, _, a map[string]any, _ string) { delete(a, "fork_turns") }},
		{"full history", func(_ *testing.T, _, a map[string]any, _ string) { a["fork_turns"] = "all" }},
		{"fork context", func(_ *testing.T, _, a map[string]any, _ string) { delete(a, "fork_turns"); a["fork_context"] = true }},
		{"explicit model", func(_ *testing.T, _, a map[string]any, _ string) { a["model"] = "user-model" }},
		{"explicit effort", func(_ *testing.T, _, a map[string]any, _ string) { a["reasoning_effort"] = "low" }},
		{"custom role", func(_ *testing.T, _, a map[string]any, _ string) { a["agent_type"] = "explorer" }},
		{"blank task", func(_ *testing.T, _, a map[string]any, _ string) { a["message"] = "  " }},
		{"wrong tool", func(_ *testing.T, p, _ map[string]any, _ string) { p["tool_name"] = "send_message" }},
		{"wrong vendor", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(hookspec.AgentEnv, VendorClaude) }},
		{"unknown profile", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(hookspec.CodexProfileEnv, "missing") }},
		{"non Databricks profile", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(hookspec.CodexProfileEnv, "openai") }},
		{"unsupported parent", func(_ *testing.T, p, _ map[string]any, _ string) { p["model"] = "databricks-claude-sonnet-5-5" }},
		{"environment model pin", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(providers.PinCodexModel, "user-model") }},
		{"environment effort pin", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(providers.PinCodexEffort, "low") }},
		{"vault model pin", func(t *testing.T, _, _ map[string]any, _ string) {
			vaulttest.Put(t, vault.Pins, providers.PinCodexModel, "user-model")
		}},
		{"vault effort pin", func(t *testing.T, _, _ map[string]any, _ string) {
			vaulttest.Put(t, vault.Pins, providers.PinCodexEffort, "low")
		}},
		{"missing catalog", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(hookspec.CodexCatalogEnv, "") }},
		{"hidden candidate", func(t *testing.T, _, _ map[string]any, path string) {
			if err := os.WriteFile(path, []byte(`{"models":[{"slug":"`+codexRouteWireModel+`","visibility":"hide"}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"unlisted candidate", func(t *testing.T, _, _ map[string]any, path string) {
			if err := os.WriteFile(path, []byte(`{"models":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			catalog := setupCodexRoute(t)
			args := map[string]any{"fork_turns": "none", "message": "add a --verbose flag"}
			payload := codexRoutePayload(args)
			tc.edit(t, payload, args, catalog)
			before := string(mustCodexJSON(t, payload))
			if got := runCodexRoute(t, payload); got != nil {
				t.Fatalf("ineligible spawn was rewritten: %#v", got)
			}
			if string(mustCodexJSON(t, payload)) != before {
				t.Fatal("original arguments mutated")
			}
		})
	}
}

func TestCodexAgentRouteMalformedInputFailsOpen(t *testing.T) {
	setupCodexRoute(t)
	for _, body := range []string{"", "not-json", "{}", `{"tool_name":"spawn_agent","tool_input":null}`} {
		var out, stderr bytes.Buffer
		if code := CodexAgentRoute(strings.NewReader(body), &out, &stderr); code != ExitProceed || out.Len() != 0 {
			t.Errorf("input %q: exit %d, output %q", body, code, &out)
		}
	}
}

func mustCodexJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
