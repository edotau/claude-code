package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	_ "github.com/edotau/claude-code/internal/claude" // registers the claude row an unsigned agent's pick falls through to
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runAgentEffort returns the decoded updatedInput (nil when the hook emitted nothing) and the exit code.
func runAgentEffort(t *testing.T, stdin string) (map[string]any, int) {
	t.Helper()
	var out bytes.Buffer
	code := runEffortHook(strings.NewReader(stdin), &out)
	if strings.TrimSpace(out.String()) == "" {
		return nil, code
	}
	var doc agentEffortDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("emitted invalid JSON (%v): %q", err, out.String())
	}
	if doc.HSO.Event != "PreToolUse" {
		t.Errorf("hookEventName = %q, want PreToolUse", doc.HSO.Event)
	}
	return doc.HSO.UpdatedInput, code
}

// agentEffortDoc is the full emitted payload, for tests asserting systemMessage and additionalContext.
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

// effortHome points CLAUDE_CONFIG_DIR at a fresh dir (no ambient recall hit, no real roster) holding the agent specs.
func effortHome(t *testing.T, agents ...string) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, a := range agents {
		writeAgentFile(t, cfg, a)
	}
	return cfg
}

func TestAgentEffortSystemMessageAndContextBothPresent(t *testing.T) {
	effortHome(t)
	doc := runAgentEffortFull(t, `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text"}}`)
	if want := "dispatch-effort: low → haiku (mechanical)"; doc.SystemMessage != want {
		t.Errorf("systemMessage = %q, want %q", doc.SystemMessage, want)
	}
	if doc.HSO.AdditionalContext == "" {
		t.Error("additionalContext must still be present alongside systemMessage")
	}
	if doc.HSO.UpdatedInput["model"] != "haiku" {
		t.Errorf("updatedInput[model] = %v, want haiku", doc.HSO.UpdatedInput["model"])
	}
}

// The hook is advisory plumbing and must never block.
func TestAgentEffortAlwaysProceeds(t *testing.T) {
	effortHome(t)
	for _, stdin := range []string{``, `not-json`, `{}`, `{"tool_name":"Agent"}`, `{"tool_name":"Agent","tool_input":{}}`,
		`{"tool_name":"Agent","tool_input":{"prompt":"fix a typo"}}`} {
		if _, code := runAgentEffort(t, stdin); code != core.ExitProceed {
			t.Errorf("stdin %q exited %d, want ExitProceed", stdin, code)
		}
	}
}

// wantModel "" means the dispatch must be left untouched: explicit and roster-pinned models win, other tools and
// path-shaped types are ignored, and high complexity is pinned to sonnet rather than inheriting.
func TestAgentEffortModelPin(t *testing.T) {
	effortHome(t, "pinned-agent:sonnet", "inherit-agent:inherit")
	cases := []struct{ name, stdin, wantModel string }{
		{"low -> haiku", `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text"}}`, "haiku"},
		{"medium -> sonnet", `{"tool_name":"Agent","tool_input":{"prompt":"add a --verbose flag to the token command"}}`, "sonnet"},
		{"high capped at sonnet", `{"tool_name":"Agent","tool_input":{"prompt":"refactor the auth ladder and migrate the hooks"}}`, "sonnet"},
		{"explicit model wins", `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","model":"opus"}}`, ""},
		{"inherit is unset", `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text","model":"inherit"}}`, "haiku"},
		{"other tool ignored", `{"tool_name":"Bash","tool_input":{"prompt":"fix a typo","command":"ls"}}`, ""},
		{"Task tool filled", `{"tool_name":"Task","tool_input":{"prompt":"fix a typo"}}`, "haiku"},
		{"empty subagent_type", `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in README","subagent_type":""}}`, "haiku"},
		{"path-shaped type untouched", `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","subagent_type":"../etc/agents/x"}}`, ""},
		{"roster pin wins", `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","subagent_type":"pinned-agent"}}`, ""},
		{"roster inherit = session model", `{"tool_name":"Agent","tool_input":{"prompt":"fix a typo","subagent_type":"inherit-agent"}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runAgentEffort(t, c.stdin)
			if c.wantModel == "" && got != nil {
				t.Errorf("dispatch was rewritten to %v, want untouched", got)
			}
			if c.wantModel != "" && (got == nil || got["model"] != c.wantModel) {
				t.Errorf("updatedInput = %v, want model=%q", got, c.wantModel)
			}
		})
	}
}

// updatedInput replaces the whole input record, so every original key must survive.
func TestAgentEffortPreservesAllInputKeys(t *testing.T) {
	effortHome(t)
	in := `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text","description":"fix typo",` +
		`"isolation":"worktree","run_in_background":false,"subagent_type":"general-purpose"}}`
	got, _ := runAgentEffort(t, in)
	want := map[string]any{"prompt": "fix the typo in the help text", "description": "haiku: fix typo", "isolation": "worktree",
		"run_in_background": false, "subagent_type": "general-purpose", "model": "haiku"}
	if len(got) != len(want) {
		t.Fatalf("updatedInput has %d keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("updatedInput[%q] = %v, want %v", k, got[k], v)
		}
	}
}

// seedHomeRecallBlock writes n id-marked home-bank blocks repeating the query terms so BM25 scores them close.
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

// recallDispatch is an Agent payload carrying prompt plus extra raw JSON fields.
func recallDispatch(prompt, extra string) string {
	return fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"prompt":%q%s}}`, prompt, extra)
}

func TestAgentEffortExplicitModelRecallAppendsPromptOnly(t *testing.T) {
	effortHome(t)
	seedHomeRecallBlock(t, 1)
	doc := runAgentEffortFull(t, recallDispatch(recallPrompt, `,"model":"opus"`))
	if doc.HSO.UpdatedInput["model"] != "opus" {
		t.Errorf("model = %v, want untouched \"opus\"", doc.HSO.UpdatedInput["model"])
	}
	if p, _ := doc.HSO.UpdatedInput["prompt"].(string); !strings.Contains(p, "<memory-recall>") {
		t.Errorf("prompt missing the recall block: %q", p)
	}
	if doc.SystemMessage != "" {
		t.Errorf("systemMessage = %q, want empty — the model never changed", doc.SystemMessage)
	}
}

// An empty bank plus an explicit model means no rewrite applies: nothing is emitted.
func TestAgentEffortNoHitsNoPinEmitsNothing(t *testing.T) {
	effortHome(t)
	if got, code := runAgentEffort(t, recallDispatch(recallPrompt, `,"model":"opus"`)); got != nil || code != core.ExitProceed {
		t.Errorf("updatedInput = %v code=%d, want nil/ExitProceed", got, code)
	}
}

// A prompt already carrying a <memory-recall> block must not get a second one.
func TestAgentEffortRecallSkipsPromptAlreadyCarryingMarker(t *testing.T) {
	effortHome(t)
	seedHomeRecallBlock(t, 1)
	already := recallPrompt + "\n\n<memory-recall>\n- old hit\n</memory-recall>"
	if got, _ := runAgentEffort(t, recallDispatch(already, `,"model":"opus"`)); got != nil {
		t.Errorf("prompt already carrying a recall block was rewritten: %v", got)
	}
}

// The model fill and the recall append can both fire on one dispatch without dropping an original key.
func TestAgentEffortRecallPreservesAllInputKeysWithModelFill(t *testing.T) {
	effortHome(t)
	seedHomeRecallBlock(t, 1)
	got, _ := runAgentEffort(t, recallDispatch(recallPrompt,
		`,"description":"deploy work","isolation":"worktree","run_in_background":false,"subagent_type":"general-purpose"`))
	if got == nil {
		t.Fatal("emitted nothing, want an updatedInput")
	}
	want := map[string]any{"description": "sonnet: deploy work", "isolation": "worktree", "run_in_background": false, "subagent_type": "general-purpose"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("updatedInput[%q] = %v, want %v (original key dropped)", k, got[k], v)
		}
	}
	if got["model"] == nil {
		t.Error("model was not filled alongside the recall append")
	}
	if p, _ := got["prompt"].(string); !strings.Contains(p, "<memory-recall>") {
		t.Errorf("prompt missing the recall block: %q", p)
	}
}

// A recall engine that cannot read the bank (config dir is a file) still fills the model and never blocks.
func TestAgentEffortRecallEngineFailureFallsBackToTodaysBehavior(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", notADir)
	got, code := runAgentEffort(t, `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text"}}`)
	if code != core.ExitProceed {
		t.Errorf("exit = %d, want ExitProceed despite an unreadable bank", code)
	}
	if got == nil || got["model"] != "haiku" {
		t.Errorf("updatedInput = %v, want model=haiku despite the recall failure", got)
	}
}

// effortResult runs the pin step on a payload without the emitter.
func effortResult(payload string) core.Result {
	return agentEffortResult(core.ParseInput(strings.NewReader(payload)))
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
	res := effortResult(in)
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
	labelCell = func(_ *dispatchRun, agent, alias string) string { return agent + "-" + alias }
	payload := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"model":"sonnet","prompt":%q,"description":%q}}`,
		subagentType, prompt, desc)
	r := effortResult(payload)
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
	if got != "sonnet: review thing" {
		t.Fatalf("label = %q, want %q", got, "sonnet: review thing")
	}
	// Running twice (the labelled description fed back in) does not double-prefix.
	if again := labelOf(t, "claude", prompt, got); again != got {
		t.Errorf("second run = %q, want unchanged %q", again, got)
	}
	if got := labelOf(t, "Explore", prompt, "review thing"); got != "sonnet: review thing" {
		t.Errorf("Explore label = %q, want its own model prefix", got)
	}
	cases := []struct{ st, agent, want string }{
		{"claude", "copilot", "copilot-sonnet: task"},
		{"copilot", "claude", "copilot-sonnet: task"},
		{"claude", "claude (in-context)", "sonnet: task"},
		{"claude", "claude", "sonnet: task"},
		{"codex", "codex → gemini", "codex-sonnet: task"},
	}
	// Mode: compare joins with ", "; relay (explicit or absent) keeps the arrow.
	for _, c := range []struct{ prompt, want string }{
		{"Mode: compare\nAgent: codex, gemini", "sonnet: task"},
		{"Agent: codex, gemini\nMode: Compare", "sonnet: task"},
		{"Mode: relay\nAgent: codex → gemini", "sonnet: task"},
		{"Agent: codex → gemini", "sonnet: task"},
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
	useAgentsDir(t, "claude", "copilot", "Plan")
	lower := []struct{ st, agent, desc, want string }{
		{"claude", "copilot", "Fix bug", "copilot-sonnet: fix bug"},
		{"claude", "copilot", "copilot: Fix bug", "copilot-sonnet: fix bug"},
		{"claude", "none", "Exercising live-session cap", "sonnet: exercising live-session cap"},
		{"claude", "none", "HTTP retry", "sonnet: HTTP retry"},
		{"claude", "copilot", "CLAUDE.md rewrite", "copilot-sonnet: CLAUDE.md rewrite"},
		{"claude", "none", "internal_x fix", "sonnet: internal_x fix"},
		{"claude", "none", "Fix v2 bug", "sonnet: fix v2 bug"},
		{"claude", "none", "V2 bug", "sonnet: V2 bug"},
		{"claude", "none", "Done. Next", "sonnet: done. Next"},
		{"claude", "none", "claude: RTK hook", "sonnet: RTK hook"},
		{"copilot", "none", "copilot: repair tests", "copilot-sonnet: repair tests"},
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
		"Agent: claude (in-context)\nreview": "gpt-6.1-sol: review diff", // an external type runs its agent, whatever the brief
	} {
		r := effortResult(payload(prompt))
		if got, _ := r.UpdatedInput["description"].(string); got != want && (want != "review diff" || r.UpdatedInput != nil) {
			t.Errorf("%q: label = %q, want %q", prompt, got, want)
		}
	}
	// A known profile is validated upstream by applyProfile; the model comes from that profile's alias map.
	if got := labelModel(newDispatchRun(nil), map[string]any{"model": "sonnet"}, "claude", "Agent: codex\nProfile: qa"); got != "databricks-gpt-6-astra" {
		t.Errorf("profile model = %q, want the profile's sonnet row", got)
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
	r := effortResult(autoDispatch("claude", "fix bug"))
	if r.Decision != core.Block || r.UpdatedInput != nil || !strings.Contains(r.Reason, `"sonnet: fix bug"`) {
		t.Fatalf("unprefixed auto dispatch = %+v, want a Block naming \"sonnet: fix bug\" and no rewrite", r)
	}
	r = effortResult(autoDispatch("claude", "sonnet: fix bug"))
	if r.Decision == core.Block {
		t.Fatalf("resend with the right prefix was blocked: %+v", r)
	}
	manual := strings.Replace(autoDispatch("claude", "fix bug"), `"permission_mode":"auto",`, "", 1)
	r = effortResult(manual)
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
		r := effortResult(payload)
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
	r := effortResult(payload)
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
	r := effortResult(payload)
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
				t.Setenv(router.EnvCoworker, "databricks:dev")
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

// useAgentsDir points CLAUDE_CONFIG_DIR at a fresh dir whose agents/ holds one file per name (`name: model` sets a model).
func useAgentsDir(t *testing.T, names ...string) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	for _, n := range names {
		writeAgentFile(t, cfg, n)
	}
	return cfg
}

// writeAgentFile writes agents/<name>.md; a "name:model" argument also sets its frontmatter model.
func writeAgentFile(t *testing.T, cfg, spec string) {
	t.Helper()
	name, model, _ := strings.Cut(spec, ":")
	body := "---\nname: " + name + "\n"
	if model != "" {
		body += "model: " + model + "\n"
	}
	if err := os.MkdirAll(filepath.Join(cfg, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "agents", name+".md"), []byte(body+"---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// allowedTypes are the dispatch types the inventory covers: the seven-file roster shape plus the built-ins.
var allowedTypes = []string{"claude", "codex", "copilot", "gemini", "opencode", "Explore", "Plan", "general-purpose"}

// inventoryLabel is the label oracle: an external type shows its stubbed pick "<agent>-<tier>", a Claude-run type its model.
func inventoryLabel(st, model string) string {
	if externalTypes[st] {
		return st + "-" + model
	}
	return model
}

// TestDispatchLabelInventory: every allowed type × {explicit, agent-file, classified} model × {plain, correct, wrong,
// doubled} description ends as exactly one `<model>: ` prefix; auto mode blocks only a missing or wrong prefix.
func TestDispatchLabelInventory(t *testing.T) {
	const task = "review the diff"
	for _, st := range allowedTypes {
		for _, src := range []string{"explicit", "file", "none"} {
			cfg := useAgentsDir(t, "claude", "codex", "copilot", "gemini", "opencode", "Plan")
			stubLabelModels(t, "", "")
			labelCell = func(_ *dispatchRun, agent, alias string) string { return agent + "-" + alias }
			model := map[string]string{"explicit": "opus", "file": "haiku"}[src]
			if src == "file" {
				writeAgentFile(t, cfg, st+":haiku")
			}
			pinned, _, _ := dispatchLabel(LabelInput{Type: st, Model: map[string]string{"explicit": "opus"}[src], Brief: task})
			if src == "none" {
				_, model, _ = dispatchLabel(LabelInput{Type: st, Brief: task})
			}
			label := inventoryLabel(st, model)
			if pinned != label {
				t.Errorf("%s/%s: agents label = %q, want %q", st, src, pinned, label)
			}
			want := label + ": " + task
			for kind, desc := range map[string]string{"plain": task, "correct": want, "wrong": "gpt-4-1-mini: " + task,
				"doubled": label + ": " + want, "d1": "sonnet: databricks-gpt-6-1-sol: " + task} {
				assertInventoryRow(t, st, src, kind, desc, want)
			}
		}
	}
}

// assertInventoryRow runs one dispatch outside and inside auto mode and checks the description and the block.
func assertInventoryRow(t *testing.T, st, src, kind, desc, want string) {
	t.Helper()
	in := map[string]any{"subagent_type": st, "prompt": "review the diff", "description": desc}
	if src == "explicit" {
		in["model"] = "opus"
	}
	for _, auto := range []bool{false, true} {
		doc := map[string]any{"tool_name": "Agent", "tool_input": in}
		if auto {
			doc["permission_mode"] = "auto"
		}
		raw, _ := json.Marshal(doc)
		r := agentEffortResult(core.ParseInput(bytes.NewReader(raw)))
		got, _ := r.UpdatedInput["description"].(string)
		if got == "" {
			got = desc
		}
		blocked := r.Decision == core.Block
		switch {
		case auto && blocked != (kind != "correct"):
			t.Errorf("%s/%s/%s auto: blocked = %v (%s)", st, src, kind, blocked, r.Reason)
		case auto && blocked && !strings.Contains(r.Reason, fmt.Sprintf("%q", want)):
			t.Errorf("%s/%s/%s auto: reason %q, want it to name %q", st, src, kind, r.Reason, want)
		case !auto && (blocked || got != want):
			t.Errorf("%s/%s/%s: description %q (blocked %v), want %q", st, src, kind, got, blocked, want)
		}
	}
}

// D1: a codex leaf whose Agent: line mentions claude-code was labelled with the wrapper's alias stacked on the
// routed model; an external type's label is its own pick, once.
func TestDispatchLabelExternalTypeIgnoresAgentLineWords(t *testing.T) {
	useAgentsDir(t, "codex")
	stubLabelModels(t, "databricks-gpt-6-1-sol", "")
	payload := `{"tool_name":"Agent","permission_mode":"auto","tool_input":{"subagent_type":"codex","model":"sonnet",` +
		`"prompt":"Agent: codex (run via claude-code agents ask --agent codex)\nreview","description":"databricks-gpt-6-1-sol: review phase 2"}}`
	if r := effortResult(payload); r.Decision == core.Block {
		t.Fatalf("correctly labelled codex dispatch was blocked: %s", r.Reason)
	}
}

// The allowed types are the agents/ files plus the built-ins: adding or removing a file changes the set, nothing else does.
func TestDispatchTypeGateFollowsAgentFiles(t *testing.T) {
	cfg := useAgentsDir(t, "claude", "codex", "Plan")
	blocked := func(st, brief string) string {
		raw, _ := json.Marshal(map[string]any{"tool_name": "Agent", "tool_input": map[string]any{"subagent_type": st, "prompt": brief, "description": "x"}})
		r := agentEffortResult(core.ParseInput(bytes.NewReader(raw)))
		if r.Decision != core.Block || !strings.HasPrefix(r.Reason, "dispatch-effort: subagent_type") {
			return ""
		}
		return r.Reason
	}
	for _, st := range []string{"claude", "codex", "Plan", "Explore", "general-purpose", ""} {
		if r := blocked(st, "do it"); r != "" {
			t.Errorf("%q blocked: %s", st, r)
		}
	}
	const roster = "(Explore, Plan, claude, codex, general-purpose)"
	for st, c := range map[string]struct{ brief, use string }{
		"copilot": {"do it", `use "general-purpose"`}, "web-fetch": {"read a page", `use "general-purpose"`},
		"claude-code-guide": {"q", `use "general-purpose"`}, "statusline-setup": {"q", `use "general-purpose"`},
		"claude-context-optimizer:context-analyzer": {"q", `use "general-purpose"`},
		"coworker:copilot":                          {"do it", `use "copilot"`},
		"coworker:opencode":                         {"do it", `use "opencode"`},
		"coworker:databricks":                       {"Agent: codex\nProfile: dev\ndo it", `use "codex"`},
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

// An agents dir that cannot be read or holds no agent never blocks the roster: it falls back to agentid plus the built-ins.
func TestDispatchTypeGateFallsBackToRoster(t *testing.T) {
	orig := listAgents
	t.Cleanup(func() { listAgents = orig })
	listAgents = func(string) ([]project.Source, error) { return nil, errors.New("permission denied") }
	if got, want := dispatchTypes(), []string{"Explore", "claude", "codex", "copilot", "gemini", "general-purpose", "opencode"}; !slices.Equal(got, want) {
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
	r := pre(`{"tool_name":"Agent","tool_use_id":"gate1","tool_input":{"subagent_type":"web-fetch","prompt":"x","description":"x"}}`)
	if r.Decision != core.Block || len(markerNames(t)) != len(before) || hasMarker(t, "gate1") {
		t.Fatalf("web-fetch = %+v, markers %v -> %v; want a block and no new marker", r, before, markerNames(t))
	}
	if r := pre(`{"tool_name":"Agent","tool_use_id":"gate2","tool_input":{"subagent_type":"claude","model":"opus","prompt":"x","description":"x"}}`); r.Decision == core.Block || !hasMarker(t, "gate2") {
		t.Fatalf("claude = %+v, want a claimed slot (negative control)", r)
	}
}

func stubProfiles(t *testing.T, names []string, ok bool) {
	t.Helper()
	old := profileNames
	profileNames = func(*dispatchRun) ([]string, bool) { return names, ok }
	oldRow := servingRow
	servingRow = func(*dispatchRun, string) error { return nil }
	t.Cleanup(func() { profileNames, servingRow = old, oldRow })
}

func profileSpawn(t *testing.T, kind, prompt string) string {
	t.Helper()
	in := map[string]any{"subagent_type": kind, "prompt": prompt, "description": "fix thing"}
	b, err := json.Marshal(map[string]any{"tool_name": "Agent", "session_id": "s", "tool_input": in})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Any type with a Profile: line keeps its type and gets the exact ask command prepended, once.
func TestProfileBriefKeepsTypeIdempotently(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubProfiles(t, []string{"dbx", "openai"}, true)
	for _, kind := range []string{"general-purpose", "claude", "copilot"} {
		brief := "Agent: codex\nProfile: dbx\nreview a.go"
		res := effortResult(profileSpawn(t, kind, brief))
		if res.Decision == core.Block || res.UpdatedInput == nil {
			t.Fatalf("%s: res = %+v, want an update", kind, res)
		}
		if got := res.UpdatedInput["subagent_type"]; got != kind {
			t.Errorf("%s: subagent_type = %v, want the type kept", kind, got)
		}
		p, _ := res.UpdatedInput["prompt"].(string)
		if !strings.HasPrefix(p, "Run: claude-code agents ask --agent codex --profile dbx") || !strings.HasSuffix(p, brief) {
			t.Errorf("%s: prompt = %q, want the Run: line then the brief", kind, p)
		}
		again := effortResult(profileSpawn(t, "claude", p))
		if q, _ := again.UpdatedInput["prompt"].(string); again.UpdatedInput != nil && q != p {
			t.Errorf("%s: second pass changed the prompt:\n%q\nvs\n%q", kind, q, p)
		}
	}
}

func TestProfileBriefDefaultsToClaudeAsk(t *testing.T) {
	updated := map[string]any{"subagent_type": "general-purpose", "prompt": "Profile: dbx\ndo it"}
	stubProfiles(t, []string{"dbx"}, true)
	note, deny := applyProfile(newDispatchRun(nil), updated, "prompt", "Profile: dbx\ndo it")
	if deny != nil || strings.Contains(note, "moved to") || updated["subagent_type"] != "general-purpose" || !strings.HasPrefix(updated["prompt"].(string), "Run: claude-code agents ask --agent claude --profile dbx") {
		t.Fatalf("note=%q deny=%v updated=%v", note, deny, updated)
	}
}

// A sentence-ending period after the name ("Profile: dbx.") still routes to dbx, not an unknown "dbx.".
func TestProfileTrailingPeriodIsNotPartOfName(t *testing.T) {
	stubProfiles(t, []string{"dbx"}, true)
	updated := map[string]any{"subagent_type": "claude", "prompt": "Profile: dbx.\ndo it"}
	if note, deny := applyProfile(newDispatchRun(nil), updated, "prompt", "Profile: dbx.\ndo it"); deny != nil || !strings.Contains(note, "--profile dbx`") {
		t.Fatalf("note=%q deny=%v", note, deny)
	}
}

// An unknown profile is a Block that lists every valid name; an unreadable registry fails open.
func TestProfileUnknownDeniedAndRegistryFailureOpen(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	brief := "Profile: nosuch\ndo it"
	stubProfiles(t, []string{"dbx", "openai"}, true)
	res := effortResult(profileSpawn(t, "general-purpose", brief))
	if res.Decision != core.Block || !strings.Contains(res.Reason, `"nosuch"`) || !strings.Contains(res.Reason, "dbx, openai") {
		t.Fatalf("unknown profile: res = %+v, want a Block naming nosuch and listing dbx, openai", res)
	}
	stubProfiles(t, nil, false)
	res = effortResult(profileSpawn(t, "general-purpose", brief))
	if res.Decision == core.Block || res.UpdatedInput["subagent_type"] != "general-purpose" || !strings.HasPrefix(res.UpdatedInput["prompt"].(string), "Run: ") {
		t.Fatalf("registry unreadable: res = %+v, want fail open and the ask command on the kept type", res)
	}
}

// Read-only types and in-context briefs keep their type and get no ask command.
func TestProfileIgnoredForReadOnlyAndInContext(t *testing.T) {
	stubProfiles(t, []string{"dbx"}, true)
	for kind, brief := range map[string]string{
		"Plan": "Profile: dbx\nplan it", "Explore": "Agent: codex\nProfile: dbx\nlook", "gemini": "Profile: dbx\nlook",
		"general-purpose": "Agent: in-context\nProfile: dbx\ndo it",
	} {
		updated := map[string]any{"subagent_type": kind, "prompt": brief}
		note, deny := applyProfile(newDispatchRun(nil), updated, "prompt", brief)
		if deny != nil || !strings.Contains(note, "ignored") || updated["subagent_type"] != kind || updated["prompt"] != brief {
			t.Errorf("%s: note=%q deny=%v updated=%v, want type and prompt untouched", kind, note, deny, updated)
		}
	}
}

// TestArmProfileLineBlocked: under databricks:dev a Profile: line for another profile is blocked, dev's own passes.
func TestArmProfileLineBlocked(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(router.EnvCoworker, "databricks:dev")
	stubProfiles(t, []string{"dev", "qa"}, true)
	res := effortResult(profileSpawn(t, "claude", "Agent: codex\nProfile: qa\nfix"))
	if res.Decision != core.Block || !strings.Contains(res.Reason, "dispatch-effort: Profile: qa conflicts with databricks:dev") {
		t.Errorf("Profile: qa = %+v, want a Block naming databricks:dev", res)
	}
	res = effortResult(profileSpawn(t, "claude", "Agent: codex\nProfile: dev\nfix"))
	if p, _ := res.UpdatedInput["prompt"].(string); res.Decision == core.Block || !strings.HasPrefix(p, "Run: claude-code agents ask --agent codex --profile dev") {
		t.Errorf("Profile: dev = %+v, want the usual Run: rewrite", res)
	}
}

// coworkerSpawn runs one dispatch of type kind with brief prompt and returns the result.
func coworkerSpawn(t *testing.T, kind, prompt string) core.Result {
	t.Helper()
	return effortResult(profileSpawn(t, kind, prompt))
}

// A Coworker: line makes each external roster leaf run `agents ask --overlay`; a bare one takes the session's profile.
func TestCoworkerLineRewritesExternalTypes(t *testing.T) {
	useAgentsDir(t, "claude", "codex", "copilot", "gemini", "opencode")
	stubProfiles(t, []string{"dbx", "qa"}, true)
	stubLabelModels(t, "", "")
	t.Setenv(providers.EnvSessionProfile, "")
	for _, kind := range []string{"codex", "copilot", "gemini", "opencode", "claude"} {
		res := coworkerSpawn(t, kind, "Coworker: databricks:dbx\nreview a.go")
		prompt, _ := res.UpdatedInput["prompt"].(string)
		if want := "Run: claude-code agents ask --agent " + kind + " " + `--overlay '{"provider":"databricks","profile":"dbx"}' `; res.Decision == core.Block || !strings.HasPrefix(prompt, want) {
			t.Errorf("%s: prompt %q (reason %q), want it to start %q", kind, prompt, res.Reason, want)
		}
	}
	if r := coworkerSpawn(t, "codex", "Coworker: databricks\nx"); r.Decision != core.Block || !strings.Contains(r.Reason, "needs a workspace") {
		t.Errorf("bare Coworker with no session profile = %+v, want a block", r)
	}
	t.Setenv(providers.EnvSessionProfile, "qa")
	if p, _ := coworkerSpawn(t, "codex", "Coworker: databricks\nx").UpdatedInput["prompt"].(string); !strings.Contains(p, `--overlay '{"provider":"databricks","profile":"qa"}' `) {
		t.Errorf("bare Coworker with session profile qa: prompt %q", p)
	}
	if p, _ := coworkerSpawn(t, "Explore", "Coworker: databricks:dbx\nx").UpdatedInput["prompt"].(string); strings.Contains(p, "--overlay") {
		t.Errorf("Explore must ignore Coworker:, prompt %q", p)
	}
}

// Coworker: blocks an unknown profile, a different Profile:, and leaving an armed session; the armed one is a no-op.
func TestCoworkerLineConflicts(t *testing.T) {
	useAgentsDir(t, "codex")
	stubProfiles(t, []string{"dbx", "qa"}, true)
	stubLabelModels(t, "", "")
	for brief, want := range map[string]string{
		"Coworker: databricks:nope\nx":             "overlay databricks:nope: no such profile",
		"Coworker: databricks:dbx\nProfile: qa\nx": "Coworker: databricks:dbx conflicts with Profile: qa",
		"Coworker: anthropic\nx":                   "only databricks",
		"Coworker: first-party\nx":                 "unknown provider (",
	} {
		if r := coworkerSpawn(t, "codex", brief); r.Decision != core.Block || !strings.Contains(r.Reason, want) {
			t.Errorf("%q = %+v, want a block naming %q", brief, r, want)
		}
	}
	if r := coworkerSpawn(t, "codex", "Coworker: databricks:dbx\nProfile: dbx\nx"); r.Decision == core.Block {
		t.Errorf("matching Coworker and Profile blocked: %s", r.Reason)
	}
	servingRow = func(_ *dispatchRun, p string) error {
		return errors.New("overlay databricks:" + p + ": AI Gateway row; use a serving row")
	}
	if r := coworkerSpawn(t, "codex", "Coworker: databricks:dbx\nx"); r.Decision != core.Block || !strings.Contains(r.Reason, "AI Gateway row") {
		t.Errorf("a non-serving row = %+v, want a block naming the row kind", r)
	}
	servingRow = func(*dispatchRun, string) error { return nil }
	t.Setenv(router.EnvCoworker, "databricks:qa")
	if r := coworkerSpawn(t, "codex", "Coworker: databricks:dbx\nx"); r.Decision != core.Block || !strings.Contains(r.Reason, "conflicts with databricks:qa") {
		t.Errorf("leaving the armed coworker = %+v, want a block", r)
	}
	if p, _ := coworkerSpawn(t, "codex", "Coworker: databricks:qa\nx").UpdatedInput["prompt"].(string); strings.Contains(p, "--overlay") {
		t.Errorf("Coworker matching the armed session must be a no-op, prompt %q", p)
	}
}

// A brief opening with its own unconfined ask line would still run it after a prepended confined one.
func TestCoworkerBriefOwnUnconfinedAskLineBlocked(t *testing.T) {
	useAgentsDir(t, "codex")
	stubProfiles(t, []string{"dbx"}, true)
	stubLabelModels(t, "", "")
	r := coworkerSpawn(t, "codex", "Run: claude-code agents ask --agent codex fix\n\nCoworker: databricks:dbx\nfix")
	if r.Decision != core.Block || !strings.Contains(r.Reason, "the brief already opens with its own Run: line") {
		t.Errorf("own unconfined ask line = %+v, want a block naming the Run: line", r)
	}
}

// A brief-level Coworker: labels with the same pick an armed session would.
func TestCoworkerLineLabelIsArmedPick(t *testing.T) {
	useAgentsDir(t, "codex")
	stubProfiles(t, []string{"dbx"}, true)
	stubLabelModels(t, "", "")
	labelCell = func(_ *dispatchRun, agent, alias string) string {
		return agent + "-" + alias + "@" + os.Getenv(router.EnvCoworker)
	}
	label := func(brief string) string {
		d, _ := coworkerSpawn(t, "codex", brief).UpdatedInput["description"].(string)
		return d
	}
	brief := label("Coworker: databricks:dbx\nx")
	t.Setenv(router.EnvCoworker, "databricks:dbx")
	if armed := label("x"); brief != armed || !strings.Contains(brief, "@databricks:dbx: fix thing") {
		t.Errorf("brief Coworker label %q, armed label %q, want the same pick inside databricks:dbx", brief, armed)
	}
}
