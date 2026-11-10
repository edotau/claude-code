package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/claude" // registers the claude row an unsigned agent's pick falls through to
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runAgentEffort runs the hook, returning the decoded updatedInput (nil when the hook emitted
// nothing) and the exit code.
func runAgentEffort(t *testing.T, stdin string) (map[string]any, int) {
	t.Helper()
	var out bytes.Buffer
	code := runEffortHook(strings.NewReader(stdin), &out)
	if strings.TrimSpace(out.String()) == "" {
		return nil, code
	}
	var doc struct {
		HSO struct {
			Event        string         `json:"hookEventName"`
			UpdatedInput map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("emitted invalid JSON (%v): %q", err, out.String())
	}
	if doc.HSO.Event != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", doc.HSO.Event)
	}
	return doc.HSO.UpdatedInput, code
}

// agentEffortDoc is the full emitted payload shape, for tests asserting systemMessage and
// additionalContext alongside updatedInput.
type agentEffortDoc struct {
	SystemMessage string `json:"systemMessage"`
	HSO           struct {
		Event             string         `json:"hookEventName"`
		AdditionalContext string         `json:"additionalContext"`
		UpdatedInput      map[string]any `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
}

func runAgentEffortFull(t *testing.T, stdin string) agentEffortDoc {
	t.Helper()
	var out bytes.Buffer
	runEffortHook(strings.NewReader(stdin), &out)
	var doc agentEffortDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("emitted invalid JSON (%v): %q", err, out.String())
	}
	return doc
}

// TestAgentEffortSystemMessageAndContextBothPresent: R2 adds a top-level systemMessage beside the
// existing additionalContext — neither replaces the other, and additionalContext keeps its own note.
func TestAgentEffortSystemMessageAndContextBothPresent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	doc := runAgentEffortFull(t, `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text"}}`)
	if want := "agent-effort: low → haiku (mechanical)"; doc.SystemMessage != want {
		t.Errorf("systemMessage = %q, want %q", doc.SystemMessage, want)
	}
	if doc.HSO.AdditionalContext == "" {
		t.Error("additionalContext must still be present alongside systemMessage")
	}
	if doc.HSO.UpdatedInput["model"] != "haiku" {
		t.Errorf("updatedInput[model] = %v, want haiku", doc.HSO.UpdatedInput["model"])
	}
}

// The hook must never block — it is advisory plumbing, not a gate.
func TestAgentEffortAlwaysProceeds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	for _, stdin := range []string{
		``,
		`not-json`,
		`{}`,
		`{"tool_name":"Agent"}`,
		`{"tool_name":"Agent","tool_input":{}}`,
		`{"tool_name":"Agent","tool_input":{"prompt":"fix a typo"}}`,
	} {
		if _, code := runAgentEffort(t, stdin); code != core.ExitProceed {
			t.Errorf("stdin %q exited %d, want ExitProceed", stdin, code)
		}
	}
}

func TestAgentEffortFillsUnsetModel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	cases := []struct{ name, prompt, wantModel string }{
		{"low complexity -> haiku", "fix the typo in the help text", "haiku"},
		{"medium complexity -> sonnet", "add a --verbose flag to the token command", "sonnet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := `{"tool_name":"Agent","tool_input":{"prompt":"` + c.prompt + `"}}`
			got, _ := runAgentEffort(t, in)
			if got == nil {
				t.Fatal("emitted nothing, want an updatedInput with a model")
			}
			if got["model"] != c.wantModel {
				t.Errorf("model = %v, want %q", got["model"], c.wantModel)
			}
		})
	}
}

// High complexity is PINNED to sonnet, not left to inherit. Inheriting meant a verbose prompt silently
// landed on whatever the parent session ran, which is how long audits ended up on the reasoning tier
// with no rule naming them. Whoever genuinely wants a bigger model sets model: explicitly — that wins.
func TestAgentEffortCapsHighAtSonnet(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	in := `{"tool_name":"Agent","tool_input":{"prompt":"refactor the auth ladder and migrate the hooks"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil {
		t.Fatal("high-complexity dispatch was left to inherit the session model, want pinned model=sonnet")
	}
	if got["model"] != "sonnet" {
		t.Errorf("model = %v, want \"sonnet\"", got["model"])
	}
}

// An explicit model is a deliberate choice by whoever dispatched; the hook must not second-guess it
// even when the prompt classifies cheap.
func TestAgentEffortRespectsExplicitModel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","model":"opus"}}`
	if got, _ := runAgentEffort(t, in); got != nil {
		t.Errorf("explicit model was overridden to %v, want untouched", got)
	}
}

// The dispatch's own `model:"inherit"` is a Claude Code default, not a deliberate pin — it must be
// treated as unset, or a spawn left at "inherit" silently ran on the parent session's model instead
// of the classifier's cost-down tier.
func TestAgentEffortTreatsInheritModelAsUnset(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text","model":"inherit"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil {
		t.Fatal("model:\"inherit\" was left untouched, want it filled like an unset model")
	}
	if got["model"] != "haiku" {
		t.Errorf("model = %v, want \"haiku\"", got["model"])
	}
}

// A roster agent that pins a concrete model already carries its tier — defer to the frontmatter.
// Hermetic: the roster is fabricated in a temp CLAUDE_CONFIG_DIR — reading the real repo roster
// would pass locally (ambient CLAUDE_CONFIG_DIR = the checkout) but fail in CI (empty ~/.claude).
func TestAgentEffortDefersToRosterPin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	writeAgent := func(name, model string) {
		body := "---\nname: " + name + "\ndescription: test agent\nmodel: " + model + "\n---\nbody\n"
		if err := os.MkdirAll(filepath.Join(home, "agents"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "agents", name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeAgent("pinned-agent", "sonnet")
	writeAgent("inherit-agent", "inherit")

	pinned := `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","subagent_type":"pinned-agent"}}`
	if got, _ := runAgentEffort(t, pinned); got != nil {
		t.Errorf("model-pinned agent was overridden to %v, want untouched", got)
	}
	inherit := `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","subagent_type":"inherit-agent"}}`
	if got, _ := runAgentEffort(t, inherit); got != nil {
		t.Errorf("inherit-model agent was overridden to %v, want untouched (frontmatter inherit = session model)", got)
	}
}

// updatedInput REPLACES the whole input record, so dropping a key would silently alter the dispatch.
// This is the change's sharpest edge — assert every original key survives byte-for-byte.
func TestAgentEffortPreservesAllInputKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	in := `{"tool_name":"Agent","tool_input":{` +
		`"prompt":"fix the typo in the help text",` +
		`"description":"fix typo",` +
		`"isolation":"worktree",` +
		`"run_in_background":false,` +
		`"subagent_type":"general-purpose"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil {
		t.Fatal("emitted nothing, want an updatedInput")
	}
	want := map[string]any{
		"prompt":            "fix the typo in the help text",
		"description":       "haiku: fix typo", // labelled with the filled model
		"isolation":         "worktree",
		"run_in_background": false,
		"subagent_type":     "general-purpose",
		"model":             "haiku", // the only added key
	}
	if len(got) != len(want) {
		t.Fatalf("updatedInput has %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("updatedInput[%q] = %v, want %v", k, got[k], v)
		}
	}
}

// A non-spawn tool must be ignored even if the matcher is ever widened by mistake.
func TestAgentEffortIgnoresOtherTools(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	in := `{"tool_name":"Bash","tool_input":{"prompt":"fix a typo","command":"ls"}}`
	if got, _ := runAgentEffort(t, in); got != nil {
		t.Errorf("non-spawn tool was rewritten to %v, want untouched", got)
	}
}

// The matcher is "Agent|Task" (settings.Registry), and core.SpawnTools names both — Task
// must be filled exactly like Agent, not treated as inert.
func TestAgentEffortFillsUnsetModelOnTaskTool(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	in := `{"tool_name":"Task","tool_input":{"prompt":"fix a typo"}}`
	got, _ := runAgentEffort(t, in)
	if got == nil || got["model"] != "haiku" {
		t.Errorf("Task-tool dispatch = %v, want model=haiku", got)
	}
}

// With no subagent_type at all (a fresh general-purpose agent), the classifier still applies.
func TestAgentEffortNoSubagentType(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in README","subagent_type":""}}`
	got, _ := runAgentEffort(t, in)
	if got == nil || got["model"] != "haiku" {
		t.Errorf("model = %v, want haiku", got)
	}
}

// A path-shaped subagent_type is not a roster name; the dispatch is left untouched rather than guessed at.
func TestAgentEffortRefusesPathShapedSubagentType(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // hermetic: no recall hit from an ambient real bank
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","subagent_type":"../etc/agents/x"}}`
	if got, _ := runAgentEffort(t, in); got != nil {
		t.Errorf("path-shaped subagent_type was rewritten to %v, want untouched", got)
	}
}

// seedHomeRecallBlock writes n id-marked, strongly-matching home-bank blocks so AgentRecall has
// real hits to rank; each block repeats the query terms plus padding so BM25 scores them close.
func seedHomeRecallBlock(t *testing.T, n int) {
	t.Helper()
	if err := os.MkdirAll(memory.HomeBankDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("# Conventions\n\n")
	for i := range n {
		fmt.Fprintf(&b, "- deploy pipeline staging signoff rule %d %s<!-- id:%08x -->\n", i, strings.Repeat("padding ", 40), i+1)
	}
	if err := os.WriteFile(filepath.Join(memory.HomeBankDir(), "conventions.md"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

const recallPrompt = "stage the deploy pipeline staging signoff rollout"

// TestAgentEffortExplicitModelRecallAppendsPromptOnly: an explicit model still gets the recall
// block (prompt changes), the model field is untouched, and no systemMessage/decision fires since
// the model itself never changed.
func TestAgentEffortExplicitModelRecallAppendsPromptOnly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seedHomeRecallBlock(t, 1)
	in := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"prompt":%q,"model":"opus"}}`, recallPrompt)
	doc := runAgentEffortFull(t, in)
	if doc.HSO.UpdatedInput["model"] != "opus" {
		t.Errorf("model = %v, want untouched \"opus\"", doc.HSO.UpdatedInput["model"])
	}
	prompt, _ := doc.HSO.UpdatedInput["prompt"].(string)
	if !strings.Contains(prompt, "<memory-recall>") {
		t.Errorf("prompt missing the recall block: %q", prompt)
	}
	if doc.SystemMessage != "" {
		t.Errorf("systemMessage = %q, want empty — the model never changed", doc.SystemMessage)
	}
}

// TestAgentEffortNoHitsNoPinEmitsNothing: an empty bank plus an explicit model means neither
// rewrite applies, so the hook must emit nothing at all (not an empty-but-present updatedInput).
func TestAgentEffortNoHitsNoPinEmitsNothing(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no bank seeded: nothing to recall
	in := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"prompt":%q,"model":"opus"}}`, recallPrompt)
	if got, code := runAgentEffort(t, in); got != nil || code != core.ExitProceed {
		t.Errorf("updatedInput = %v code=%d, want nil/ExitProceed", got, code)
	}
}

// TestAgentEffortRecallSkipsPromptAlreadyCarryingMarker: a prompt that already has a <memory-recall>
// block (e.g. relayed from a parent turn) must not get a second one appended.
func TestAgentEffortRecallSkipsPromptAlreadyCarryingMarker(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seedHomeRecallBlock(t, 1)
	already := recallPrompt + "\n\n" + "<memory-recall>" + "\n- old hit\n</memory-recall>"
	in := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"prompt":%q,"model":"opus"}}`, already)
	got, _ := runAgentEffort(t, in)
	if got != nil {
		t.Errorf("prompt already carrying a recall block was rewritten: %v", got)
	}
}

// TestAgentEffortRecallPreservesAllInputKeysWithModelFill: the model-fill path AND the recall-append
// path can both fire on the same dispatch, and every original key still survives alongside both edits.
func TestAgentEffortRecallPreservesAllInputKeysWithModelFill(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seedHomeRecallBlock(t, 1)
	in := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{`+
		`"prompt":%q,"description":"deploy work","isolation":"worktree",`+
		`"run_in_background":false,"subagent_type":"general-purpose"}}`, recallPrompt)
	got, _ := runAgentEffort(t, in)
	if got == nil {
		t.Fatal("emitted nothing, want an updatedInput")
	}
	for _, k := range []string{"description", "isolation", "run_in_background", "subagent_type"} {
		want := map[string]any{"description": "sonnet: deploy work", "isolation": "worktree", "run_in_background": false, "subagent_type": "general-purpose"}[k]
		if got[k] != want {
			t.Errorf("updatedInput[%q] = %v, want %v (original key dropped)", k, got[k], want)
		}
	}
	if got["model"] == nil {
		t.Error("model was not filled alongside the recall append")
	}
	prompt, _ := got["prompt"].(string)
	if !strings.Contains(prompt, "<memory-recall>") {
		t.Errorf("prompt missing the recall block: %q", prompt)
	}
}

// TestAgentEffortRecallEngineFailureFallsBackToTodaysBehavior: if the recall engine can't read the
// bank (CLAUDE_CONFIG_DIR points at a file, not a directory), the hook must still fill the model and
// never crash or block — the same fail-open contract as the decision-log write.
func TestAgentEffortRecallEngineFailureFallsBackToTodaysBehavior(t *testing.T) {
	home := t.TempDir()
	notADir := filepath.Join(home, "blocked")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", notADir)
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text"}}`
	got, code := runAgentEffort(t, in)
	if code != core.ExitProceed {
		t.Errorf("exit = %d, want ExitProceed despite an unreadable bank", code)
	}
	if got == nil || got["model"] != "haiku" {
		t.Errorf("updatedInput = %v, want model=haiku despite the recall failure", got)
	}
}

// runEffortHook renders the pin step through the production PreToolUse emitter.
func runEffortHook(r io.Reader, stdout io.Writer) int {
	return core.Emit(core.AgentClaude, "PreToolUse", agentEffortResult(core.ParseInput(r)), stdout, os.Stderr)
}

// A pinned agent's label names the pinned model, not the route table's cell (real labelCell, hermetic vault).
func TestLabelCellPinnedModelBeatsRouteCell(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"probe":{"preset":"databricks","host":"https://example.invalid","auth":{"type":"none"}}}}}`)
	cell := labelCell(newDispatchRun(nil), "codex", "sonnet")
	pathstest.FakeVendor(t, "codex", "exit 0") // the label is the pick: an uninstalled codex would fall through to claude
	vaulttest.SetRecord(t, "codex", "profile", "probe")
	vaulttest.SetRecord(t, "codex", "model", "databricks-gpt-pin")
	got := labelCell(newDispatchRun(nil), "codex", "sonnet")
	if got != "databricks-gpt-pin" {
		t.Errorf("labelCell(codex) = %q with the pin set (unpinned cell %q), want the pinned model", got, cell)
	}
}

// TestAgentEffortAutoModeRecallRidesContextNotUpdatedInput: in auto mode a recall-only result must carry no
// updatedInput (the classifier returns no verdict on a rewritten call); the recall goes to additionalContext.
func TestAgentEffortAutoModeRecallRidesContextNotUpdatedInput(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seedHomeRecallBlock(t, 2)
	in := fmt.Sprintf(`{"tool_name":"Agent","permission_mode":"auto","tool_input":{"prompt":%q,"model":"opus"}}`, recallPrompt)
	res := agentEffortResult(core.ParseInput(strings.NewReader(in)))
	if res.UpdatedInput != nil {
		t.Errorf("updatedInput = %v, want none in auto mode", res.UpdatedInput)
	}
	if !strings.Contains(res.Context, "memory-recall for this dispatch") {
		t.Errorf("context = %q, want the recall block", res.Context)
	}
}

func labelOf(t *testing.T, subagentType, prompt, desc string) string {
	t.Helper()
	stubLabelModels(t, "", "")
	payload := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"model":"sonnet","prompt":%q,"description":%q}}`,
		subagentType, prompt, desc)
	r := agentEffortResult(core.ParseInput(strings.NewReader(payload)))
	if r.UpdatedInput == nil {
		return desc
	}
	d, _ := r.UpdatedInput["description"].(string)
	return d
}

// stubLabelModels makes every vendor leaf's planned model cell (route table) or prof (profile), never live lookups.
func stubLabelModels(t *testing.T, cell, prof string) {
	t.Helper()
	oc, op := labelCell, labelProfileModel
	t.Cleanup(func() { labelCell, labelProfileModel = oc, op })
	labelCell = func(*dispatchRun, string, string) string { return cell }
	labelProfileModel = func(*dispatchRun, string, string) string { return prof }
}

func TestDispatchAgentLabels(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	prompt := "Agent: gemini → codex\nreview the thing"
	got := labelOf(t, "claude", prompt, "review thing")
	if got != "gemini → codex: review thing" {
		t.Fatalf("label = %q, want %q", got, "gemini → codex: review thing")
	}
	// Running twice (the labelled description fed back in) does not double-prefix.
	if again := labelOf(t, "claude", prompt, got); again != got {
		t.Errorf("second run = %q, want unchanged %q", again, got)
	}
	if got := labelOf(t, "Explore", prompt, "review thing"); got != "sonnet: review thing" {
		t.Errorf("Explore label = %q, want its own model prefix", got)
	}
	cases := []struct{ st, agent, want string }{
		{"claude", "copilot", "copilot: task"},
		{"copilot", "claude", "sonnet: task"},
		{"claude", "claude (in-context)", "sonnet: task"},
		{"claude", "claude", "sonnet: task"},
		{"codex", "codex → gemini", "gemini: task"},
	}
	// Mode: compare joins with ", "; relay (explicit or absent) keeps the arrow.
	for _, c := range []struct{ prompt, want string }{
		{"Mode: compare\nAgent: codex, gemini", "codex, gemini: task"},
		{"Agent: codex, gemini\nMode: Compare", "codex, gemini: task"},
		{"Mode: relay\nAgent: codex → gemini", "codex → gemini: task"},
		{"Agent: codex → gemini", "codex → gemini: task"},
	} {
		if got := labelOf(t, "claude", c.prompt, "task"); got != c.want {
			t.Errorf("prompt %q = %q, want %q", c.prompt, got, c.want)
		}
	}
	for _, c := range cases {
		if got := labelOf(t, c.st, "do it\nAgent: "+c.agent, "task"); got != c.want {
			t.Errorf("%s + Agent: %s = %q, want %q", c.st, c.agent, got, c.want)
		}
	}
}

// Descriptions start lowercase and drop a hand-typed prefix that repeats the row's own type, for any subagent type.
func TestDispatchLabelLowercase(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	lower := []struct{ st, agent, desc, want string }{
		{"claude", "copilot", "Fix bug", "copilot: fix bug"},
		{"claude", "copilot", "copilot: Fix bug", "copilot: fix bug"},
		{"claude", "none", "Exercising live-session cap", "sonnet: exercising live-session cap"},
		{"claude", "none", "HTTP retry", "sonnet: HTTP retry"},
		{"claude", "copilot", "CLAUDE.md rewrite", "copilot: CLAUDE.md rewrite"},
		{"claude", "none", "internal_x fix", "sonnet: internal_x fix"},
		{"claude", "none", "Fix v2 bug", "sonnet: fix v2 bug"},
		{"claude", "none", "V2 bug", "sonnet: V2 bug"},
		{"claude", "none", "Done. Next", "sonnet: done. Next"},
		{"claude", "none", "claude: RTK hook", "sonnet: RTK hook"},
		{"copilot", "none", "copilot: repair tests", "sonnet: repair tests"},
		{"Plan", "none", "Plan: Repo-wide package consolidation", "sonnet: repo-wide package consolidation"},
	}
	for _, c := range lower {
		if got := labelOf(t, c.st, "do it\nAgent: "+c.agent, c.desc); got != c.want {
			t.Errorf("%s + Agent: %s on %q = %q, want %q", c.st, c.agent, c.desc, got, c.want)
		}
	}
}

// A routed leaf's label names the agent and the model it is planned to run; a Profile: line uses that profile's model.
func TestDispatchLabelNamesRoutedModel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	payload := func(prompt string) string {
		return fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":"codex","model":"sonnet","prompt":%q,"description":"review diff"}}`, prompt)
	}
	stubLabelModels(t, "gpt-6.1-sol", "databricks-gpt-6-astra")
	for prompt, want := range map[string]string{
		"Agent: codex\nreview":               "gpt-6.1-sol: review diff",
		"Agent: claude (in-context)\nreview": "sonnet: review diff",
	} {
		r := agentEffortResult(core.ParseInput(strings.NewReader(payload(prompt))))
		if got, _ := r.UpdatedInput["description"].(string); got != want && (want != "review diff" || r.UpdatedInput != nil) {
			t.Errorf("%q: label = %q, want %q", prompt, got, want)
		}
	}
	// A known profile is validated upstream by applyProfile; the model comes from that profile's alias map.
	if got := labelModel(newDispatchRun(nil), map[string]any{"model": "sonnet"}, "claude", "Agent: codex\nProfile: qa"); got != "databricks-gpt-6-astra" {
		t.Errorf("profile model = %q, want the profile's sonnet row", got)
	}
}

func TestCoworkerLabels(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubProfiles(t, []string{"dbx"}, true)
	stubLabelModels(t, "", "dbx-model")
	labelCell = func(_ *dispatchRun, agent, alias string) string { return agent + "-" + alias }
	for _, c := range []struct{ st, prompt, want string }{
		{"coworker:copilot", "fix x", "copilot-sonnet: task"},
		{"coworker:copilot", "Agent: codex\nfix x", "copilot-sonnet: task"},
		{"coworker:opencode", "fix x", "opencode-sonnet: task"},
		{"coworker", "fix x", "copilot-sonnet: task"},
		{"coworker", "Agent: opencode\nfix x", "opencode-sonnet: task"},
		{"coworker:databricks", "Agent: codex\nProfile: dbx\nfix x", "dbx-model: task"},
	} {
		payload := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"model":"sonnet","prompt":%q,"description":"task"}}`,
			c.st, c.prompt)
		r := agentEffortResult(core.ParseInput(strings.NewReader(payload)))
		if got, _ := r.UpdatedInput["description"].(string); got != c.want {
			t.Errorf("%s %q: label = %q, want %q", c.st, c.prompt, got, c.want)
		}
	}
}

func TestDiscoveryHintPredicate(t *testing.T) {
	old := discoveryPoolSize
	discoveryPoolSize = func() int { return 6 }
	t.Cleanup(func() { discoveryPoolSize = old })
	cases := []struct {
		name, st, task string
		want           bool
	}{
		{"coworker", "coworker", "do a thing", true},
		{"coworker:copilot", "coworker:copilot", "do a thing", true},
		{"coworker:opencode", "coworker:opencode", "do a thing", true},
		{"coworker:databricks", "coworker:databricks", "do a thing", false},
		{"copilot", "copilot", "do a thing", true},
		{"opencode", "opencode", "do a thing", true},
		{"codex", "codex", "do a thing", false},
		{"agent opencode", "general-purpose", "Agent: opencode\nedit x", true},
		{"agent copilot", "general-purpose", "Agent: copilot\nrun tests", true},
		{"explicit model", "copilot", "run with --model gpt-6 now", false},
		{"other agent", "general-purpose", "Agent: codex\nreview", false},
		{"no agent line", "Explore", "look around", false},
	}
	for _, c := range cases {
		got := discoveryHint(c.st, c.task)
		if (got != "") != c.want {
			t.Errorf("%s: hint = %q, want hint=%v", c.name, got, c.want)
		}
		if got != "" && !strings.Contains(got, "(6 models)") {
			t.Errorf("%s: hint %q lacks the pool size", c.name, got)
		}
	}
}

// autoDispatch is an auto-mode Agent payload for type st with a pinned model and description desc.
func autoDispatch(st, desc string) string {
	return fmt.Sprintf(`{"tool_name":"Agent","permission_mode":"auto","tool_input":{"subagent_type":%q,"model":"sonnet",`+
		`"prompt":"do it","description":%q}}`, st, desc)
}

// In auto mode a missing model prefix is denied once with the exact description to resend (tool input is never
// rewritten there); the resend passes, and outside auto mode the hook rewrites as before.
func TestDispatchLabelAutoModeDeniesOnce(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubLabelModels(t, "", "")
	r := agentEffortResult(core.ParseInput(strings.NewReader(autoDispatch("claude", "fix bug"))))
	if r.Decision != core.Block || r.UpdatedInput != nil || !strings.Contains(r.Reason, `"sonnet: fix bug"`) {
		t.Fatalf("unprefixed auto dispatch = %+v, want a Block naming \"sonnet: fix bug\" and no rewrite", r)
	}
	r = agentEffortResult(core.ParseInput(strings.NewReader(autoDispatch("claude", "sonnet: fix bug"))))
	if r.Decision == core.Block {
		t.Fatalf("resend with the right prefix was blocked: %+v", r)
	}
	manual := strings.Replace(autoDispatch("claude", "fix bug"), `"permission_mode":"auto",`, "", 1)
	r = agentEffortResult(core.ParseInput(strings.NewReader(manual)))
	if d, _ := r.UpdatedInput["description"].(string); r.Decision == core.Block || d != "sonnet: fix bug" {
		t.Fatalf("outside auto mode = %+v, want the rewrite", r)
	}
}

// An agent-named external leaf is labelled with the model its cell resolves to; a Profile: brief on claude with its profile's.
func TestDispatchLabelExternalTypeAndProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubProfiles(t, []string{"qa"}, true)
	stubLabelModels(t, "gpt-6.1-sol", "databricks-qa-model")
	for st, c := range map[string]struct{ prompt, want string }{
		"copilot": {"do it", "gpt-6.1-sol: fix bug"}, "claude": {"Profile: qa\ndo it", "databricks-qa-model: fix bug"},
	} {
		payload := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"model":"sonnet","prompt":%q,"description":"fix bug"}}`, st, c.prompt)
		r := agentEffortResult(core.ParseInput(strings.NewReader(payload)))
		if d, _ := r.UpdatedInput["description"].(string); d != c.want {
			t.Errorf("%s label = %q, want %q", st, d, c.want)
		}
	}
}

// An `inherit` agent file labels with the session's model, read from the transcript.
func TestDispatchLabelInheritUsesSessionModel(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	stubLabelModels(t, "", "")
	if err := os.MkdirAll(filepath.Join(cfg, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "agents", "claude.md"), []byte("---\nname: claude\nmodel: inherit\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tx := filepath.Join(t.TempDir(), "session.jsonl")
	line := `{"type":"assistant","message":{"model":"claude-opus-4-7","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(tx, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"tool_name":"Agent","transcript_path":%q,"tool_input":{"subagent_type":"claude","prompt":"do it","description":"fix bug"}}`, tx)
	r := agentEffortResult(core.ParseInput(strings.NewReader(payload)))
	if d, _ := r.UpdatedInput["description"].(string); d != "claude-opus-4-7: fix bug" {
		t.Fatalf("description = %q (result %+v), want the session model prefix", d, r)
	}
}

// One dispatch-effort run reads the leaf's agent file once and loads the provider registry once, labelling as before.
func TestAgentEffortReadsRosterAndRegistryOnce(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	file := filepath.Join(cfg, "agents", "worker.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("---\nname: worker\nmodel: haiku\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reads, loads := 0, 0
	origRead, origLoad := readAgentFile, loadRegistry
	t.Cleanup(func() { readAgentFile, loadRegistry = origRead, origLoad })
	readAgentFile = func(p string) ([]byte, error) {
		if p == file {
			reads++
		}
		return origRead(p)
	}
	loadRegistry = func() (*providers.Registry, error) {
		loads++
		return &providers.Registry{Providers: map[string]*providers.Provider{"dbx": {Models: map[string]string{"haiku": "dbx-haiku"}}}}, nil
	}
	payload := `{"tool_name":"Agent","tool_input":{"subagent_type":"worker","prompt":"Profile: dbx\nfix x","description":"fix x"}}`
	r := agentEffortResult(core.ParseInput(strings.NewReader(payload)))
	if d, _ := r.UpdatedInput["description"].(string); d != "dbx-haiku: fix x" || reads != 1 || loads != 1 {
		t.Fatalf("description %q, agent-file reads %d, registry loads %d; want \"dbx-haiku: fix x\", 1, 1", d, reads, loads)
	}
}

const armDevOverlay = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`

// TestArmLabelIsPick: armed or not, the label is the no-run pick's first model sans profile prefix, not the table's.
func TestArmLabelIsPick(t *testing.T) {
	for _, armed := range []bool{true, false} {
		t.Run(fmt.Sprint("armed=", armed), func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
			vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "test-value")
			pathstest.FakeVendor(t, "codex", "exit 0")
			if armed {
				vaulttest.SetRecord(t, "codex", "profile", "dev")
				t.Setenv("HARNESS_ROUTE", "databricks:dev")
			}
			_, res, err := agentpkg.Resolve(context.Background(), agentpkg.RouteReq{Agent: "codex", Model: "sonnet", Fallback: router.FallbackAll, Preview: true})
			if err != nil || len(res.Candidates) == 0 {
				t.Fatalf("preview = %+v, %v; want a candidate (skipped %+v)", res.Candidates, err, res.Skipped)
			}
			first := res.Candidates[0]
			want := strings.TrimPrefix(first.Model, first.Profile+":")
			pins, _ := agentpkg.AgentPins()
			if predicted := tableCellModel(newDispatchRun(nil), "codex", "sonnet", pins["codex"]); armed == (want == first.Model) || !armed && want == predicted {
				t.Fatalf("fixture: pick %q (profile %q), prediction %q; want a prefixed pick armed, a pick unlike the prediction unarmed", first.Model, first.Profile, predicted)
			}
			if got := cellModelFor(newDispatchRun(nil), "codex", "sonnet"); got != want {
				t.Errorf("label = %q, want the first candidate %s on %s: %q", got, first.Agent, first.Account, want)
			}
		})
	}
}

// A pick with nothing runnable labels with the table prediction, never an empty prefix.
func TestLabelWithoutPickIsPrediction(t *testing.T) {
	vaulttest.Use(t)
	orig := labelPick
	labelPick = func(string, string) string { return "" }
	t.Cleanup(func() { labelPick = orig })
	want := tableCellModel(newDispatchRun(nil), "codex", "sonnet", router.PinSpec{})
	if got := cellModelFor(newDispatchRun(nil), "codex", "sonnet"); got == "" || got != want {
		t.Errorf("label = %q with no pick, want the table prediction %q", got, want)
	}
}

// A label is side-effect free: a discovery pin must not advance the rotation in quota.json.
func TestLabelWritesNoQuotaState(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	vaulttest.SetRecord(t, "copilot", "profile", "dev")
	vaulttest.SetRecord(t, "copilot", "model", router.DiscoveryModel)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "test-value") // with copilot installed, the pick reaches the rotation
	pathstest.FakeVendor(t, "copilot", "exit 0")
	before, _ := os.ReadFile(router.StatePath())
	cellModelFor(newDispatchRun(nil), "copilot", "sonnet")
	after, _ := os.ReadFile(router.StatePath())
	if !bytes.Equal(before, after) {
		t.Errorf("quota.json changed by a label: %q -> %q", before, after)
	}
}
