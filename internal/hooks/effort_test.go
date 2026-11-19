package hooks

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
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

// wantModel "" means the dispatch is left untouched (explicit or roster-pinned models, other tools, path-shaped types).
func TestAgentEffortModelPin(t *testing.T) {
	effortHome(t, "pinned-agent:sonnet", "inherit-agent:inherit")
	cases := []struct{ name, stdin, wantModel string }{
		{"low -> haiku", `{"tool_name":"Agent","tool_input":{"prompt":"fix the typo in the help text"}}`, "haiku"},
		{"medium -> sonnet", `{"tool_name":"Agent","tool_input":{"prompt":"add a --verbose flag to the token command"}}`, "sonnet"},
		{"ordinary multi-step -> sonnet", `{"tool_name":"Agent","tool_input":{"prompt":"refactor the retry ladder and split the hook helpers"}}`, "sonnet"},
		{"auth or migration -> opus", `{"tool_name":"Agent","tool_input":{"prompt":"refactor the auth ladder and migrate the hooks"}}`, "opus"},
		{"audit -> opus", `{"tool_name":"Agent","tool_input":{"prompt":"audit the safety-scan rules for false blocks"}}`, "opus"},
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
	if err := os.MkdirAll(memory.StoreDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("# Conventions\n\n")
	for i := range n {
		fmt.Fprintf(&b, "- deploy pipeline staging signoff rule %d %s<!-- id:%08x -->\n", i, strings.Repeat("padding ", 40), i+1)
	}
	if err := os.WriteFile(filepath.Join(memory.StoreDir(), "conventions.md"), []byte(b.String()), 0o600); err != nil {
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

// Auto mode refuses a modified input: a non-Run brief's recall rides additionalContext there, else the prompt.
func TestAgentEffortRecallByPermissionMode(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		wantNote bool
	}{{"auto", true}, {"default", false}, {"", false}} {
		t.Run(cmp.Or(tc.mode, "unset"), func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			seedHomeRecallBlock(t, 2)
			res := effortResult(fmt.Sprintf(`{"tool_name":"Agent","permission_mode":%q,"tool_input":{"prompt":%q,"model":"opus"}}`,
				tc.mode, recallPrompt))
			note := strings.Contains(res.Context, "memory-recall for this dispatch") && strings.Contains(res.Context, "<memory-recall>")
			p, _ := res.UpdatedInput["prompt"].(string)
			switch {
			case tc.wantNote && (!note || res.UpdatedInput != nil):
				t.Errorf("context %q, updatedInput %v; want the recall note and no updatedInput", res.Context, res.UpdatedInput)
			case !tc.wantNote && (note || !strings.HasPrefix(p, recallPrompt) || !strings.Contains(p, "<memory-recall>")):
				t.Errorf("context %q, prompt %q; want the recall block appended and no note", res.Context, p)
			}
		})
	}
}

func labelOf(t *testing.T, subagentType, prompt, desc string) string {
	t.Helper()
	stubLabelModels(t, "")
	stubResolve(t, tierOracle)
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
	stubResolve(t, func(agentpkg.ModelQuery) string { return cell })
}

// stubResolve replaces the resolver with f's model for each query, and the ladder with the query as typed.
func stubResolve(t *testing.T, f func(agentpkg.ModelQuery) string) {
	t.Helper()
	orig, origRun := resolveModel, runQuery
	t.Cleanup(func() { resolveModel, runQuery = orig, origRun })
	resolveModel = func(_ *providers.Registry, q agentpkg.ModelQuery, _ time.Time) (agentpkg.ModelRoute, error) {
		return agentpkg.ModelRoute{Agent: q.Agent, Model: f(q)}, nil
	}
	runQuery = func(_ context.Context, _ *providers.Registry, q agentpkg.ModelQuery, _ string) (agentpkg.ModelQuery, string, error) {
		return q, "", nil
	}
}

// tierOracle labels a query `<agent>-<model>`: the typed model, else the task tier, else "pin" (the agent's own).
func tierOracle(q agentpkg.ModelQuery) string {
	return q.Agent + "-" + cmp.Or(q.Model, q.Default, "pin")
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

// Auto mode rewrites a missing model prefix like every other mode: no deny, the prefixed description in updatedInput.
func TestDispatchLabelAutoModeRewrites(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubLabelModels(t, "")
	r := effortResult(autoDispatch("claude", "fix bug"))
	if d, _ := r.UpdatedInput["description"].(string); r.Decision == core.Block || d != "sonnet: fix bug" {
		t.Fatalf("unprefixed auto dispatch = %+v, want the rewrite to \"sonnet: fix bug\"", r)
	}
	r = effortResult(autoDispatch("claude", "sonnet: fix bug"))
	if r.Decision == core.Block || r.UpdatedInput != nil {
		t.Fatalf("right prefix = %+v, want it untouched", r)
	}
}

// A Run: brief's wrong label and coworker type are fixed in one updatedInput, any mode; its prompt is byte-identical.
func TestDispatchRunBriefFixedNotDenied(t *testing.T) {
	useAgentsDir(t, "claude", "copilot")
	stubLabelModels(t, "claude-sonnet-5.5")
	const brief = "Run: claude-code agents ask --agent copilot --profile copilot 'fix it'\n\nObjective: fix it"
	for _, tc := range []struct {
		mode, st, desc string
		notes          int // one note line per fix
	}{
		{"default", "copilot", "opus: fix it", 1},
		{"default", "copilot", "sonnet: fix it", 0}, // an equivalent label is accepted as typed
		{"auto", "copilot", "fix it", 1},
		{"auto", "coworker:copilot", "databricks-gpt-6-1-sol: fix it", 2},
		{"auto", "copilot", "claude-sonnet-5.5: fix it", 0},
	} {
		raw, _ := json.Marshal(map[string]any{"tool_name": "Agent", "permission_mode": tc.mode,
			"tool_input": map[string]any{"subagent_type": tc.st, "prompt": brief, "description": tc.desc}})
		r := agentEffortResult(context.Background(), core.ParseInput(bytes.NewReader(raw)))
		d, _ := r.UpdatedInput["description"].(string)
		fixed := tc.notes > 0
		switch {
		case r.Decision == core.Block:
			t.Errorf("%s/%s/%q denied: %s", tc.mode, tc.st, tc.desc, r.Reason)
		case fixed && (d != "claude-sonnet-5.5: fix it" || r.UpdatedInput["subagent_type"] != "copilot" || r.UpdatedInput["prompt"] != brief):
			t.Errorf("%s/%s/%q: updatedInput %v, want the label and type fixed, prompt byte-identical", tc.mode, tc.st, tc.desc, r.UpdatedInput)
		case !fixed && r.UpdatedInput != nil:
			t.Errorf("%s/%s/%q: right label rewritten: %v", tc.mode, tc.st, tc.desc, r.UpdatedInput)
		case strings.Count(r.Context, "dispatch-effort: ") != tc.notes:
			t.Errorf("%s/%s/%q: context %q, want %d fix notes", tc.mode, tc.st, tc.desc, r.Context, tc.notes)
		}
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
		label    string // the description prefix: the model as dev serves it
	}{
		{"claude-sonnet-5-5", 0, "", "databricks-claude-sonnet-5-5"},
		{"claude-nonexistent-9", 0, "dispatch-effort: preflight: served: dev lists no endpoint claude-nonexistent-9 " +
			"(no fallback for this dispatch: --fallback or AGENT_ERROR_FALLBACK). Fix the brief's `Run:` line", ""},
		{"claude-nonexistent-9", 48 * time.Hour, "", ""}, // a stale listing proves nothing: ask's own preflight decides
	}
	for _, tc := range cases {
		if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: time.Now().Add(-tc.age), Models: []router.ServedModel{
			{ID: "databricks-claude-sonnet-5-5", Ready: "READY"}}}); err != nil {
			t.Fatal(err)
		}
		brief := "Run: claude-code agents ask --agent claude --profile dev --fallback none --model " + tc.model + " 'do x'"
		r := effortResult(fmt.Sprintf(`{"tool_name":"Agent","tool_input":{"subagent_type":"general-purpose",`+
			`"prompt":%q,"description":%q}}`, brief, cmp.Or(tc.label, tc.model)+": do x"))
		got := ""
		if r.Decision == core.Block {
			got = r.Reason
		}
		if !strings.HasPrefix(got, tc.wantDeny) || (got == "") != (tc.wantDeny == "") {
			t.Errorf("%s (listing age %v): deny %q, want it to start %q", tc.model, tc.age, got, tc.wantDeny)
		}
	}
}

const armDevOverlay = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`

// A label is side-effect free: a discovery pin must not advance the rotation in quota.json.
func TestLabelWritesNoQuotaState(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	vaulttest.SetRecord(t, "copilot", "profile", "dev")
	vaulttest.SetRecord(t, "copilot", "model", "auto")
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "test-value") // with copilot installed, the pick reaches the rotation
	pathstest.FakeVendor(t, "copilot", "exit 0")
	before, _ := os.ReadFile(router.StatePath())
	labelModel(context.Background(), newDispatchRun(nil), map[string]any{}, "copilot", "review the diff")
	after, _ := os.ReadFile(router.StatePath())
	if !bytes.Equal(before, after) {
		t.Errorf("quota.json changed by a label: %q -> %q", before, after)
	}
}

// useAgentsDir points CLAUDE_CONFIG_DIR at a fresh dir whose agents/ holds one file per name.
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

// inventoryLabel is the label oracle: an external type shows "<agent>-<task tier>", else its model.
func inventoryLabel(st, model, task string) string {
	if externalTypes[st] {
		tier, _ := agentpkg.TaskTier(task) // a vendor CLI runs its pin, else the task tier, never the relay's model
		return st + "-" + tier
	}
	return model
}

// TestDispatchLabelInventory: every type × model source × description shape ends with exactly one `<model>: ` prefix.
func TestDispatchLabelInventory(t *testing.T) {
	const task = "review the diff"
	for _, st := range allowedTypes {
		for _, src := range []string{"explicit", "file", "none"} {
			cfg := useAgentsDir(t, "claude", "codex", "copilot", "gemini", "opencode", "Plan")
			stubLabelModels(t, "")
			stubResolve(t, tierOracle)
			model := map[string]string{"explicit": "opus", "file": "haiku"}[src]
			if src == "file" {
				writeAgentFile(t, cfg, st+":haiku")
			}
			pinned, _, _ := dispatchLabel(context.Background(), LabelInput{Type: st, Model: map[string]string{"explicit": "opus"}[src], Brief: task})
			if src == "none" {
				_, model, _ = dispatchLabel(context.Background(), LabelInput{Type: st, Brief: task})
			}
			label := inventoryLabel(st, model, task)
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

// assertInventoryRow runs one dispatch outside and inside auto mode and checks the description is the same rewrite.
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
		if r.Decision == core.Block || got != want {
			t.Errorf("%s/%s/%s auto=%v: description %q (blocked %v: %s), want %q", st, src, kind, auto, got, r.Decision == core.Block, r.Reason, want)
		}
	}
}

// D1: a codex leaf whose Agent: line mentions claude-code got the wrapper's alias stacked on its model; label once.
func TestDispatchLabelExternalTypeIgnoresAgentLineWords(t *testing.T) {
	useAgentsDir(t, "codex")
	stubLabelModels(t, "databricks-gpt-6-1-sol")
	payload := `{"tool_name":"Agent","permission_mode":"auto","tool_input":{"subagent_type":"codex","model":"sonnet",` +
		`"prompt":"Agent: codex (run via claude-code agents ask --agent codex)\nreview","description":"databricks-gpt-6-1-sol: review phase 2"}}`
	if r := effortResult(payload); r.Decision == core.Block {
		t.Fatalf("correctly labelled codex dispatch was blocked: %s", r.Reason)
	}
}

// A second note-mode recall in one session must not repeat pointers the first recall note already gave.
func TestAgentEffortRecallNoteSkipsPointersAlreadyGiven(t *testing.T) {
	effortHome(t)
	seedHomeRecallBlock(t, 1)
	in := core.ParseInput(strings.NewReader(`{"session_id":"11111111-2222-3333-4444-555555555555","tool_name":"Agent"}`))
	recall := func() string {
		return (&effortPlan{updated: map[string]any{}}).recall(context.Background(), in, "prompt", recallPrompt, true)
	}
	if first := recall(); !strings.Contains(first, "memory-recall for this dispatch") {
		t.Fatalf("first dispatch lacks the recall note: %q", first)
	}
	if second := recall(); strings.Contains(second, "memory-recall for this dispatch") {
		t.Errorf("second dispatch repeated the pointers: %q", second)
	}
}
