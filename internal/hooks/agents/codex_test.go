package agents

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const codexRouteWireModel = "databricks-gpt-6-astra"

func setupCodexRoute(t *testing.T) string {
	t.Helper()
	root := vaulttest.Use(t)
	t.Setenv(settings.AgentEnv, core.AgentCodex)
	t.Setenv(settings.CodexProfileEnv, "probe")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"probe":{"preset":"databricks","host":"https://example.invalid","auth":{"type":"none"}}}},"routing":{"cells":{"codex":{"sonnet":{"model":"gpt-6-astra","effort":"high"}}}}}`)
	catalog := filepath.Join(root, "catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"models":[{"slug":"`+codexRouteWireModel+`","visibility":"list"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(settings.CodexCatalogEnv, catalog)
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
	if code := runCodex("codex-agent-route", bytes.NewReader(body), &out, &stderr); code != core.ExitProceed {
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

type codexIneligibleCase struct {
	name string
	edit func(*testing.T, map[string]any, map[string]any, string)
}

// codexIneligibleSpawnCases lists edits that make a spawn ineligible for routing.
func codexIneligibleSpawnCases() []codexIneligibleCase {
	return []codexIneligibleCase{
		{"implicit fork", func(_ *testing.T, _, a map[string]any, _ string) { delete(a, "fork_turns") }},
		{"full history", func(_ *testing.T, _, a map[string]any, _ string) { a["fork_turns"] = "all" }},
		{"fork context", func(_ *testing.T, _, a map[string]any, _ string) { delete(a, "fork_turns"); a["fork_context"] = true }},
		{"explicit model", func(_ *testing.T, _, a map[string]any, _ string) { a["model"] = "user-model" }},
		{"explicit effort", func(_ *testing.T, _, a map[string]any, _ string) { a["reasoning_effort"] = "low" }},
		{"custom role", func(_ *testing.T, _, a map[string]any, _ string) { a["agent_type"] = "explorer" }},
		{"blank task", func(_ *testing.T, _, a map[string]any, _ string) { a["message"] = "  " }},
		{"wrong tool", func(_ *testing.T, p, _ map[string]any, _ string) { p["tool_name"] = "send_message" }},
		{"wrong vendor", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(settings.AgentEnv, core.AgentClaude) }},
		{"unknown profile", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(settings.CodexProfileEnv, "missing") }},
		{"non Databricks profile", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(settings.CodexProfileEnv, "openai") }},
		{"unsupported parent", func(_ *testing.T, p, _ map[string]any, _ string) { p["model"] = "databricks-claude-sonnet-5-5" }},
		{"vault model pin", func(t *testing.T, _, _ map[string]any, _ string) {
			vaulttest.SetRecord(t, "codex", "model", "user-model")
		}},
		{"vault effort pin", func(t *testing.T, _, _ map[string]any, _ string) {
			vaulttest.SetRecord(t, "codex", "effort", "low")
		}},
		{"missing catalog", func(t *testing.T, _, _ map[string]any, _ string) { t.Setenv(settings.CodexCatalogEnv, "") }},
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
}

func TestCodexAgentRouteSkipsIneligibleSpawns(t *testing.T) {
	for _, tc := range codexIneligibleSpawnCases() {
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
		if code := runCodex("codex-agent-route", strings.NewReader(body), &out, &stderr); code != core.ExitProceed || out.Len() != 0 {
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

// TestCodexRoutingSchemaStampsNewLinesAndSkipsNewer pins: unstamped lines read, new lines carry schema 1, schema-2 lines are skipped.
func TestCodexRoutingSchemaStampsNewLinesAndSkipsNewer(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(settings.AgentEnv, core.AgentCodex)
	path := filepath.Join(paths.StateDir(), "codex", fmt.Sprintf("routing-%x.jsonl", sha256.Sum256([]byte("native-session"))))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := `{"event":"session-start","session_id":"native-session","matches_router_decision":null,"at":1}` + "\n" +
		`{"schema":2,"event":"child-start","session_id":"native-session","agent_id":"future","matches_router_decision":null,"at":2}` + "\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	rows, err := readCodexRoutes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Event != "session-start" {
		t.Fatalf("rows = %+v, want only the unstamped session-start (schema 2 skipped)", rows)
	}

	runCodexAuditHandler(t, "codex-routing-start", map[string]any{"model": codexRouteWireModel})
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
	var last map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &last); err != nil {
		t.Fatal(err)
	}
	if last["schema"] != float64(1) {
		t.Errorf(`new line "schema" = %v, want 1 (line %s)`, last["schema"], lines[len(lines)-1])
	}
}

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

func runCodexAuditHandler(t *testing.T, name string, payload map[string]any) {
	t.Helper()
	payload["session_id"] = "native-session"
	payload["prompt"] = "PROMPT-SECRET-SENTINEL"
	payload["tool_input"] = map[string]any{"message": "TASK-SECRET-SENTINEL", "task_name": "NAME-SECRET-SENTINEL"}
	var out, stderr bytes.Buffer
	if code := runCodex(name, bytes.NewReader(mustCodexJSON(t, payload)), &out, &stderr); code != core.ExitProceed {
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

// codexAuditJoinEvents returns the two arrival-order halves of the spawn-result / child-start causal join.
func codexAuditJoinEvents(t *testing.T, model string) (result, child func()) {
	t.Helper()
	result = func() {
		runCodexAuditHandler(t, "codex-routing-result", map[string]any{"tool_name": "collaborationspawn_agent", "tool_use_id": "spawn-1", "tool_response": `{"agent_id":"child-uuid"}`})
	}
	child = func() {
		runCodexAuditHandler(t, "codex-subagent-start", map[string]any{"agent_id": "child-uuid", "model": model})
	}
	return result, child
}

// requireCodexCausalVerification asserts the single verified row for spawn-1 / child-uuid and its model match.
func requireCodexCausalVerification(t *testing.T, verified []codexRouteRecord, model string) {
	t.Helper()
	if len(verified) != 1 {
		t.Fatalf("verified rows = %+v", verified)
	}
	got := verified[0]
	if got.ToolUseID != "spawn-1" || got.AgentID != "child-uuid" || got.Model != model || got.RequestedModel != codexRouteWireModel || got.Matches == nil || *got.Matches != (model == codexRouteWireModel) {
		t.Fatalf("causal verification = %+v", got)
	}
}

func TestCodexAuditCausalJoinBothArrivalOrders(t *testing.T) {
	for _, order := range []string{"result first", "child first"} {
		for _, model := range []string{codexRouteWireModel, "databricks-gpt-6-sol"} {
			t.Run(order+"/"+model, func(t *testing.T) {
				setupCodexRoute(t)
				if runCodexRoute(t, codexRoutePayload(map[string]any{"fork_turns": "none", "message": "add a --verbose flag"})) == nil {
					t.Fatal("fixture did not route")
				}
				result, child := codexAuditJoinEvents(t, model)
				first, second := child, result
				if order == "result first" {
					first, second = result, child
				}
				first()
				rows, _ := readCodexAuditFixture(t)
				if len(verifiedCodexRows(rows)) != 0 {
					t.Fatal("verified before the causal chain was complete")
				}
				second()
				rows, body := readCodexAuditFixture(t)
				requireCodexCausalVerification(t, verifiedCodexRows(rows), model)
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
	runCodexAuditHandler(t, "codex-routing-start", map[string]any{"model": "databricks-gpt-6-1-sol"})
	runCodexAuditHandler(t, "codex-routing-result", map[string]any{"tool_name": "collaborationspawn_agent", "tool_use_id": "spawn-1", "tool_response": `{"task_name":"/root/probe_child"}`})
	runCodexAuditHandler(t, "codex-subagent-start", map[string]any{"agent_id": "native-child-uuid", "model": codexRouteWireModel})
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
	runCodexAuditHandler(t, "codex-subagent-start", map[string]any{"agent_id": "child-b", "model": codexRouteWireModel})
	runCodexAuditHandler(t, "codex-subagent-start", map[string]any{"agent_id": "child-a", "model": codexRouteWireModel})
	rows, _ := readCodexAuditFixture(t)
	if len(verifiedCodexRows(rows)) != 0 {
		t.Fatal("same model was used as a causal join")
	}
	runCodexAuditHandler(t, "codex-routing-result", map[string]any{"tool_name": "spawn_agent", "tool_use_id": "spawn-a", "tool_response": map[string]any{"agent_id": "child-a"}})
	runCodexAuditHandler(t, "codex-routing-result", map[string]any{"tool_name": "spawn_agent", "tool_use_id": "spawn-b", "tool_response": map[string]any{"agent_id": "child-b"}})
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
			runCodexAuditHandler(t, "codex-routing-result", result)
			runCodexAuditHandler(t, "codex-subagent-start", child)
			rows, _ := readCodexAuditFixture(t)
			if len(verifiedCodexRows(rows)) != 0 {
				t.Fatalf("verified missing %s: %+v", missing, rows)
			}
		})
	}
}

func TestCodexAuditIgnoresOtherVendorsAndMissingSessions(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, vendor := range []string{core.AgentClaude, core.AgentCopilot, core.AgentCodex} {
		t.Setenv(settings.AgentEnv, vendor)
		for _, name := range []string{"codex-routing-start", "codex-routing-result", "codex-subagent-start"} {
			payload := map[string]any{"tool_name": "spawn_agent", "agent_id": "child", "model": codexRouteWireModel}
			if vendor != core.AgentCodex {
				payload["session_id"] = "session"
			}
			var out, stderr bytes.Buffer
			if runCodex(name, bytes.NewReader(mustCodexJSON(t, payload)), &out, &stderr) != core.ExitProceed {
				t.Fatal("audit blocked")
			}
		}
	}
	files, err := filepath.Glob(filepath.Join(paths.StateDir(), "codex", "routing-*.jsonl"))
	if err != nil || len(files) != 0 {
		t.Fatalf("unexpected audit files = %v, error %v", files, err)
	}
}

const (
	routeDecisionLine = `{"event":"decision","session_id":"s","tool_use_id":"t1","requested_model":"m","matches_router_decision":null,"at":1}` + "\n"
	routeResultLine   = `{"event":"tool-result","session_id":"s","tool_use_id":"t1","agent_id":"a1","matches_router_decision":null,"at":2}` + "\n"
)

// appendAndJoin appends the child-start to a pre-damaged file and asserts the join still verifies.
func appendAndJoin(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	child := codexRouteRecord{Event: "child-start", SessionID: "s", AgentID: "a1", Model: "m", At: 3}
	if err := appendCodexRoute(path, child); err != nil {
		t.Fatalf("appendCodexRoute on damaged file: %v", err)
	}
	rows, err := readCodexRoutes(path)
	if err != nil {
		t.Fatalf("readCodexRoutes after append: %v", err)
	}
	verified, children := countCodexJoinRows(t, rows)
	if verified != 1 || children != 1 {
		t.Fatalf("verified=%d child-start=%d, want 1 and 1; rows=%+v", verified, children, rows)
	}
}

// countCodexJoinRows counts verified and child-start rows, asserting every verified row matches.
func countCodexJoinRows(t *testing.T, rows []codexRouteRecord) (verified, children int) {
	t.Helper()
	for _, r := range rows {
		switch r.Event {
		case "verified":
			verified++
			if r.Matches == nil || !*r.Matches {
				t.Errorf("verified row = %+v, want matches", r)
			}
		case "child-start":
			children++
		}
	}
	return verified, children
}

func TestCodexRoutesTruncatedLastLine(t *testing.T) {
	appendAndJoin(t, routeDecisionLine+routeResultLine+`{"event":"child-sta`)
}

func TestCodexRoutesMalformedMiddleLine(t *testing.T) {
	appendAndJoin(t, routeDecisionLine+"{not json}\n"+routeResultLine)
}

func TestCodexRoutesLongLine(t *testing.T) {
	long := `{"event":"note","session_id":"s","matches_router_decision":null,"signal":"` + strings.Repeat("x", 100*1024) + `","at":1}` + "\n"
	appendAndJoin(t, routeDecisionLine+long+routeResultLine)
}

func TestCodexSpawnPinnedEnvVsVault(t *testing.T) {
	vaulttest.Use(t)
	in := core.ParseInput(strings.NewReader(`{"session_id":"s","tool_input":{"message":"x"}}`))
	if codexSpawnPinned(in) {
		t.Error("process-env CODEX_MODEL with no vault pin vetoed routing, want proceed")
	}
	vaulttest.SetRecord(t, "codex", "model", "gpt-vault")
	if !codexSpawnPinned(in) {
		t.Error("vault CODEX_MODEL pin did not veto routing")
	}
}

// readCodexRoutes skips unparsable lines and an unterminated trailing record.
func readCodexRoutes(path string) ([]codexRouteRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseCodexRoutes(data), nil
}

// codexHooks are the codex-* modules by name, with the hookspec event each renders for.
var codexHooks = map[string]struct {
	event string
	run   core.Runner
}{
	"codex-agent-route":    {settings.EventPreToolUse, CodexAgentRoute},
	"codex-routing-start":  {settings.EventSessionStart, CodexRoutingStart},
	"codex-routing-result": {settings.EventPostToolUse, CodexRoutingResult},
	"codex-subagent-start": {settings.EventSubagentStart, CodexSubagentStart},
}

// runCodex runs one codex module on stdin and renders its Result as the vendor wire output, as the hook verb does.
func runCodex(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	h := codexHooks[name]
	in := core.ParseInput(stdin)
	return core.Emit(in.Agent(), h.event, h.run(in), stdout, stderr)
}
