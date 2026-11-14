package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	_ "github.com/edotau/claude-code/internal/claude" // registers the claude row an unsigned agent's pick falls through to
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/proc"
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
	if want := "dispatch-effort: low → haiku effort=low (mechanical)"; doc.SystemMessage != want {
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
		"run_in_background": false, "subagent_type": "general-purpose", "model": "haiku", "effort": "low"}
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
	return agentEffortResult(context.Background(), core.ParseInput(strings.NewReader(payload)))
}

// runEffortHook renders the pin step through the production PreToolUse emitter.
func runEffortHook(r *strings.Reader, stdout *bytes.Buffer) int {
	return core.Emit(core.AgentClaude, "PreToolUse", agentEffortResult(context.Background(), core.ParseInput(r)), stdout, os.Stderr)
}

// A pinned agent's label names the pinned model, not the route table's cell (real labelCell, hermetic vault).
func TestLabelCellPinnedModelBeatsRouteCell(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"probe":{"preset":"databricks","host":"https://example.invalid","auth":{"type":"none"}}}}}`)
	cell := labelCell(context.Background(), newDispatchRun(nil), "codex", "sonnet")
	pathstest.FakeVendor(t, "codex", "exit 0") // the label is the pick: an uninstalled codex would fall through to claude
	vaulttest.SetRecord(t, "codex", "profile", "probe")
	vaulttest.SetRecord(t, "codex", "model", "databricks-gpt-pin")
	got := labelCell(context.Background(), newDispatchRun(nil), "codex", "sonnet")
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
	stubLabelModels(t, "")
	labelCell = func(_ context.Context, _ *dispatchRun, agent, alias string) string { return agent + "-" + alias }
	payload := fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":%q,"model":"sonnet","prompt":%q,"description":%q}}`,
		subagentType, prompt, desc)
	r := effortResult(payload)
	if r.UpdatedInput == nil {
		return desc
	}
	d, _ := r.UpdatedInput["description"].(string)
	return d
}

// stubLabelModels makes every vendor leaf's planned model cell (route table), never a live lookup.
func stubLabelModels(t *testing.T, cell string) {
	t.Helper()
	oc := labelCell
	t.Cleanup(func() { labelCell = oc })
	labelCell = func(context.Context, *dispatchRun, string, string) string { return cell }
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

// A routed leaf's label names the agent and the model it is planned to run.
func TestDispatchLabelNamesRoutedModel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	payload := func(prompt string) string {
		return fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":"codex","model":"sonnet","prompt":%q,"description":"review diff"}}`, prompt)
	}
	stubLabelModels(t, "gpt-6.1-sol")
	for prompt, want := range map[string]string{
		"Agent: codex\nreview":               "gpt-6.1-sol: review diff",
		"Agent: claude (in-context)\nreview": "gpt-6.1-sol: review diff", // an external type runs its agent, whatever the brief
	} {
		r := effortResult(payload(prompt))
		if got, _ := r.UpdatedInput["description"].(string); got != want && (want != "review diff" || r.UpdatedInput != nil) {
			t.Errorf("%q: label = %q, want %q", prompt, got, want)
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
	stubLabelModels(t, "")
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

// An `inherit` agent file labels with the session's model, read from the transcript.
func TestDispatchLabelInheritUsesSessionModel(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	stubLabelModels(t, "")
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

// One dispatch-effort run reads the leaf's agent file once and loads the provider registry at most once.
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
	payload := `{"tool_name":"Agent","tool_input":{"subagent_type":"worker","prompt":"fix x","description":"fix x"}}`
	r := effortResult(payload)
	if d, _ := r.UpdatedInput["description"].(string); d != "haiku: fix x" || reads != 1 || loads > 1 {
		t.Fatalf("description %q, agent-file reads %d, registry loads %d; want \"haiku: fix x\", 1, <=1", d, reads, loads)
	}
}

// A Run: line's model a fresh cached listing does not serve is denied with ask's preflight text; stale passes.
func TestDispatchPreflightRefusesUnservedModel(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(agentconfig.OverlayVar, "")
	t.Setenv(router.EnvFallback, "")
	stubLabelModels(t, "")
	cases := []struct {
		model    string
		age      time.Duration
		wantDeny string
	}{
		{"claude-sonnet-5-5", 0, ""},
		{"claude-nonexistent-9", 0, "dispatch-effort: preflight: served: dev lists no endpoint claude-nonexistent-9 " +
			"(no fallback for this dispatch: --fallback or AGENT_ERROR_FALLBACK)"},
		{"claude-nonexistent-9", 48 * time.Hour, ""}, // a stale listing proves nothing: ask's own preflight decides
	}
	for _, tc := range cases {
		if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: time.Now().Add(-tc.age), Models: []router.ServedModel{
			{ID: "databricks-claude-sonnet-5-5", Ready: "READY"}}}); err != nil {
			t.Fatal(err)
		}
		brief := "Run: claude-code agents ask --agent claude --profile dev --fallback none --model " + tc.model + " 'do x'"
		r := effortResult(fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":"general-purpose",`+
			`"prompt":%q,"description":%q}}`, brief, tc.model+": do x"))
		got := ""
		if r.Decision == core.Block {
			got = r.Reason
		}
		if got != tc.wantDeny {
			t.Errorf("%s (listing age %v): deny %q, want %q", tc.model, tc.age, got, tc.wantDeny)
		}
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
				t.Setenv(agentconfig.OverlayVar, `{"provider":"databricks","profile":"dev"}`)
			}
			_, res, err := agentpkg.Resolve(context.Background(), agentpkg.RouteReq{Agent: "codex", Model: "sonnet", Fallback: router.FallbackAll, Preview: true})
			if err != nil || len(res.Candidates) == 0 {
				t.Fatalf("preview = %+v, %v; want a candidate (skipped %+v)", res.Candidates, err, res.Skipped)
			}
			first := res.Candidates[0]
			want := strings.TrimPrefix(first.Model, first.Profile+":")
			pins, _ := agentpkg.AgentPinsContext(context.Background())
			if predicted := tableCellModel(newDispatchRun(nil), "codex", "sonnet", pins["codex"]); armed == (want == first.Model) || !armed && want == predicted {
				t.Fatalf("fixture: pick %q (profile %q), prediction %q; want a prefixed pick armed, a pick unlike the prediction unarmed", first.Model, first.Profile, predicted)
			}
			if got := cellModelFor(context.Background(), newDispatchRun(nil), "codex", "sonnet"); got != want {
				t.Errorf("label = %q, want the first candidate %s on %s: %q", got, first.Agent, first.Account, want)
			}
		})
	}
}

// Under a non-Databricks confinement the label is the confined pick, never the pinned databricks row's model.
func TestLabelCellConfinedProviderLabelsConfinedPick(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay) // openai·chatgpt is a built-in row: codex's own login
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "test-value")
	vaulttest.SetRecord(t, "codex", "profile", "dev")
	pathstest.FakeVendor(t, "codex", "exit 0")
	t.Setenv(agentconfig.OverlayVar, "")
	unconfined := cellModelFor(context.Background(), newDispatchRun(nil), "codex", "sonnet")
	t.Setenv(agentconfig.OverlayVar, `{"provider":"openai"}`)
	q := agentpkg.RouteReq{Agent: "codex", Model: "sonnet", Provider: "openai", Fallback: router.FallbackAll, Preview: true}
	_, res, err := agentpkg.Resolve(context.Background(), q) // what `ask --provider openai` runs
	if err != nil || len(res.Candidates) == 0 || !strings.HasPrefix(res.Candidates[0].Account, "openai·") {
		t.Fatalf("fixture: openai preview = %+v, %v; want an openai candidate first", res.Candidates, err)
	}
	want := res.Candidates[0].Model
	if got := cellModelFor(context.Background(), newDispatchRun(nil), "codex", "sonnet"); got != want || got == unconfined {
		t.Errorf("label under openai = %q (unconfined %q), want the confined pick %q", got, unconfined, want)
	}
}

// A pick with nothing runnable labels with the table prediction, never an empty prefix.
func TestLabelWithoutPickIsPrediction(t *testing.T) {
	vaulttest.Use(t)
	orig := labelPick
	labelPick = func(context.Context, string, string) string { return "" }
	t.Cleanup(func() { labelPick = orig })
	want := tableCellModel(newDispatchRun(nil), "codex", "sonnet", router.PinSpec{})
	if got := cellModelFor(context.Background(), newDispatchRun(nil), "codex", "sonnet"); got == "" || got != want {
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
	cellModelFor(context.Background(), newDispatchRun(nil), "copilot", "sonnet")
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

// inventoryLabel is the label oracle: an external type shows its ask's classified pick "<agent>-sonnet", else its model.
func inventoryLabel(st, model string) string {
	if externalTypes[st] {
		return st + "-sonnet" // the ask classifies "review the diff" as sonnet, whatever the relay's own model
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
			stubLabelModels(t, "")
			labelCell = func(_ context.Context, _ *dispatchRun, agent, alias string) string { return agent + "-" + alias }
			model := map[string]string{"explicit": "opus", "file": "haiku"}[src]
			if src == "file" {
				writeAgentFile(t, cfg, st+":haiku")
			}
			pinned, _, _ := dispatchLabel(context.Background(), LabelInput{Type: st, Model: map[string]string{"explicit": "opus"}[src], Brief: task})
			if src == "none" {
				_, model, _ = dispatchLabel(context.Background(), LabelInput{Type: st, Brief: task})
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
		r := agentEffortResult(context.Background(), core.ParseInput(bytes.NewReader(raw)))
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
	stubLabelModels(t, "databricks-gpt-6-1-sol")
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
		r := agentEffortResult(context.Background(), core.ParseInput(bytes.NewReader(raw)))
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
		"coworker:databricks":                       {"Agent: codex\ndo it", `use "codex"`},
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
	listAgents = func(string) ([]project.Source, error) { return nil, os.ErrPermission }
	if got, want := dispatchTypes(), []string{"Explore", "claude", "codex", "copilot", "gemini", "general-purpose", "opencode"}; fmt.Sprint(got) != fmt.Sprint(want) {
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

// Concurrent picks under different confinements each see only their own overlay, and the process env never moves.
func TestCoworkerPickIsolatesConfinementsInContext(t *testing.T) {
	oc := labelCell
	t.Cleanup(func() { labelCell = oc })
	labelCell = func(ctx context.Context, _ *dispatchRun, _, _ string) string {
		before := proc.Value(ctx, agentconfig.OverlayVar)
		runtime.Gosched()
		if proc.Value(ctx, agentconfig.OverlayVar) != before {
			return "torn"
		}
		return before
	}
	env := os.Environ()
	shared := proc.WithValues(context.Background(), proc.Values{"RUN_MARK": "shared"})
	profiles := []string{"dev", "qa"}
	got := make([]string, 40)
	var wg sync.WaitGroup
	for i := range got {
		wg.Go(func() {
			got[i] = coworkerPick(shared, newDispatchRun(nil), "databricks:"+profiles[i%2], "claude", "sonnet")
		})
	}
	wg.Wait()
	for i, g := range got {
		if want := agentconfig.InlineOverlay("databricks", profiles[i%2]); g != want {
			t.Fatalf("pick %d saw overlay %q, want %q", i, g, want)
		}
	}
	if after := os.Environ(); strings.Join(after, "\x00") != strings.Join(env, "\x00") {
		t.Fatalf("process env changed across the picks: %d entries before, %d after", len(env), len(after))
	}
}

// A pick lays its confinement over the caller's run values (else the process env) in the ctx its label reads.
func TestCoworkerPickLaysConfinementOverRunValues(t *testing.T) {
	var got proc.Values
	oc := labelCell
	t.Cleanup(func() { labelCell = oc })
	labelCell = func(ctx context.Context, _ *dispatchRun, _, _ string) string {
		got, _ = proc.ValuesFromContext(ctx)
		return ""
	}
	t.Setenv("RUN_MARK", "ambient")
	run := proc.WithValues(context.Background(), proc.Values{agentconfig.OverlayVar: "outer", "RUN_MARK": "caller"})
	for name, tc := range map[string]struct {
		ctx  context.Context
		mark string
	}{"caller values": {run, "caller"}, "process env": {context.Background(), "ambient"}} {
		coworkerPick(tc.ctx, newDispatchRun(nil), "databricks:dev", "claude", "sonnet")
		if got[agentconfig.OverlayVar] != agentconfig.InlineOverlay("databricks", "dev") || got[providers.EnvSessionProfile] != "dev" || got["RUN_MARK"] != tc.mark {
			t.Errorf("%s: pick ctx = %v, want the dev confinement over RUN_MARK=%s", name, got, tc.mark)
		}
	}
}

// A second auto-mode dispatch in one session must not repeat pointers the first recall note already gave.
func TestAgentEffortRecallNoteSkipsPointersAlreadyGiven(t *testing.T) {
	effortHome(t)
	seedHomeRecallBlock(t, 1)
	payload := `{"session_id":"11111111-2222-3333-4444-555555555555","permission_mode":"auto","tool_name":"Agent",` +
		`"tool_input":{"prompt":"` + recallPrompt + `","model":"opus","description":"opus: deploy"}}`
	first := runAgentEffortFull(t, payload).HSO.AdditionalContext
	if !strings.Contains(first, "memory-recall for this dispatch") {
		t.Fatalf("first dispatch lacks the recall note: %q", first)
	}
	var out bytes.Buffer
	runEffortHook(strings.NewReader(payload), &out)
	if strings.Contains(out.String(), "memory-recall for this dispatch") {
		t.Errorf("second dispatch repeated the pointers: %q", out.String())
	}
}

func TestDispatchRunReceiptGrammar(t *testing.T) {
	for _, prefix := range []string{"", "FLAG=1 ", "timeout 30 ", "nohup ", "env FLAG=1 ", "time command ", "env FLAG=1 timeout 30 "} {
		for _, cli := range []string{"claude-code", "/opt/bin/claude-code"} {
			for _, verb := range []string{"agents ask --agent codex", "codex ask", "copilot ask", "gemini ask", "opencode ask"} {
				for _, remote := range []string{"", "--remote host "} {
					checkReceiptCommand(t, prefix+cli+" "+remote+verb+" --model pinned --fallback none 'inspect the hook'", verb, remote == "")
				}
			}
		}
	}
	for _, brief := range []string{"Run: echo 'claude-code codex ask x'", "Run: claude-code agents result id", "Run: claude-code agents tail id", "Task\nRun: claude-code codex ask x"} {
		if opensWithAsk(brief) {
			t.Errorf("not an opening launch: %q", brief)
		}
	}
}

// checkReceiptCommand expects the ask command to parse as an opening launch and to earn its run receipt and label.
func checkReceiptCommand(t *testing.T, command, verb string, local bool) {
	t.Helper()
	if !askCmdRe().MatchString(command) {
		t.Fatalf("receipt rejects %q", command)
	}
	brief := "Run: " + command
	l := parseAskLine(context.Background(), brief)
	if !opensWithAsk(brief) || l.model != "pinned" || l.prompt != "inspect the hook" || l.local != local {
		t.Fatalf("%q: parsed %+v", brief, l)
	}
	wantAgent := strings.Fields(verb)[0]
	if wantAgent == "agents" {
		wantAgent = "codex"
	}
	if l.agent != wantAgent {
		t.Fatalf("%q: agent %q", command, l.agent)
	}
	checkReceiptEarned(t, command, brief)
}

// checkReceiptEarned expects a run of command to leave a receipt and the label rule to hold for brief.
func checkReceiptEarned(t *testing.T, command, brief string) {
	t.Helper()
	var scan leafScan
	content, err := json.Marshal([]map[string]any{{"type": "tool_use", "id": "call", "name": "Bash", "input": map[string]string{"command": command}}})
	if err != nil {
		t.Fatal(err)
	}
	scan.addCalls(content)
	scan.scanResult(leafToolResult{ToolUseID: "call", Content: json.RawMessage(`"ask: ran on codex · pinned"`)})
	if !scan.ranOn {
		t.Fatalf("no receipt for %q", command)
	}
	e := &effortPlan{updated: map[string]any{"subagent_type": "codex", "description": "inspect hook"}, run: newDispatchRun(nil)}
	if denied := e.label(context.Background(), brief, true); denied == nil || !strings.Contains(denied.Reason, "pinned: inspect hook") {
		t.Fatalf("missing label enforcement for %q: %+v", brief, denied)
	}
}
