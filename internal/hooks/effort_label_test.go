package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A Run: line's --profile row spells the model as it serves it, never as typed (a typed `<row>:` is not stacked).
func TestLabelModelSpellsExplicitModelOnProfileRow(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(agentconfig.OverlayVar, "")
	for _, flags := range []string{"--profile dev --model claude-opus-5", "--model dev:claude-opus-5"} {
		task := "Run: claude-code agents ask --agent claude " + flags + " 'do x'"
		if got, _ := labelModel(context.Background(), newDispatchRun(nil), map[string]any{}, "general-purpose", task); got != "databricks-claude-opus-5" {
			t.Errorf("label for %q = %q, want databricks-claude-opus-5", flags, got)
		}
	}
}

// A Run: line's flags reach the resolver whole; an untyped ask gets the prompt's task tier below its pin.
func TestLabelModelQueryCarriesAskLine(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(agentconfig.OverlayVar, "")
	for _, tc := range []struct {
		flags                     string
		agent, model, profile, df string
		hint                      string // the alias the ladder classifies ("" = no ladder asked)
	}{
		{"--agent codex --profile dev --model sonnet --fallback none", "codex", "sonnet", "dev", "", "haiku"},
		{"--agent opencode --profile dev", "opencode", "", "dev", "haiku", "haiku"},
		{"--agent opencode --profile dev --model databricks-claude-opus-5-5", "opencode", "databricks-claude-opus-5-5", "dev", "", "haiku"},
		{"--agent codex", "codex", "", "", "haiku", "haiku"},
		{"--agent codex --model gpt-6-1-sol", "codex", "gpt-6-1-sol", "", "", "haiku"},
		{"--agent copilot --profile aueric_boehcp", "copilot", "", "aueric_boehcp", "haiku", "haiku"},
		{"--agent gemini", "gemini", "", "", "haiku", "haiku"},
		{"--agent claude --profile dev", "claude", "", "dev", "haiku", "haiku"},
	} {
		var seen agentpkg.ModelQuery
		stubResolve(t, func(q agentpkg.ModelQuery) string { seen = q; return "wire-id" })
		hint := ""
		runQuery = func(_ context.Context, _ *providers.Registry, q agentpkg.ModelQuery, alias string) (agentpkg.ModelQuery, string, error) {
			hint = alias
			return q, "", nil
		}
		task := "Run: claude-code agents ask " + tc.flags + " 'rename x'"
		if got, _ := labelModel(context.Background(), newDispatchRun(nil), map[string]any{}, "general-purpose", task); got != "wire-id" {
			t.Errorf("%s: label = %q, want the resolver's wire id", tc.flags, got)
		}
		if seen.Agent != tc.agent || seen.Model != tc.model || seen.Profile != tc.profile || seen.Default != tc.df || hint != tc.hint {
			t.Errorf("%s: query = %+v hint %q, want %s/%q/%q default %q hint %q", tc.flags, seen, hint, tc.agent, tc.model, tc.profile, tc.df, tc.hint)
		}
	}
}

// A dispatch's exact --model reaches the resolver as typed, with no default tier.
func TestLabelModelExactDispatchModel(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(agentconfig.OverlayVar, "")
	var seen agentpkg.ModelQuery
	stubResolve(t, func(q agentpkg.ModelQuery) string { seen = q; return "wire-id" })
	labelModel(context.Background(), newDispatchRun(nil), map[string]any{"model": "gpt-6-sol"}, "codex", "")
	if seen.Agent != "codex" || seen.Model != "gpt-6-sol" || seen.Default != "" {
		t.Errorf("query = %+v, want codex/gpt-6-sol with no default", seen)
	}
}

func TestLabelModelUsesCachedServedSpelling(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(agentconfig.OverlayVar, "")
	if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: time.Now(), Models: []router.ServedModel{
		{ID: "databricks-glm-5-2", Ready: "READY"}, {ID: "databricks-custom-model", Ready: "READY"},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"glm-5-2", "custom-model"} {
		task := "Run: claude-code agents ask --agent claude --profile dev --model " + model + " 'do x'"
		if got, _ := labelModel(context.Background(), newDispatchRun(nil), map[string]any{}, "general-purpose", task); got != "databricks-"+model {
			t.Errorf("label for %s = %q, want databricks-%s", model, got, model)
		}
	}
}

// A Plan leaf runs on the session model: unset it labels with the session id, an explicit pin wins.
func TestDispatchLabelPlanUsesSessionModel(t *testing.T) {
	useAgentsDir(t, "Plan")
	stubLabelModels(t, "")
	label, model, _ := dispatchLabel(context.Background(), LabelInput{Type: "Plan", SessionModel: "databricks-claude-opus-5-5"})
	if label != "databricks-claude-opus-5-5" || model != "" {
		t.Fatalf("Plan without a brief = (%q, %q), want the session id, no pin", label, model)
	}
	if label, _, _ := dispatchLabel(context.Background(), LabelInput{Type: "Plan", Model: "opus", SessionModel: "s"}); label != "opus" {
		t.Errorf("pinned Plan label = %q, want the pin opus", label)
	}
	if label, _, _ := dispatchLabel(context.Background(), LabelInput{Type: "Plan"}); label != "" {
		t.Errorf("Plan with no session = %q, want no label", label)
	}
}

// A brief with no pin is classified: the label is the classifier's model, the same as the hook writes.
func TestDispatchLabelClassifiesBrief(t *testing.T) {
	useAgentsDir(t, "Plan")
	stubLabelModels(t, "")
	label, model, _ := dispatchLabel(context.Background(), LabelInput{Type: "Plan", SessionModel: "s", Brief: "rename one variable"})
	if model == "" || label != model {
		t.Errorf("classified brief = (label %q, model %q), want the pinned model as the label", label, model)
	}
}

func TestDispatchLabelPreservesModelUnderGemini(t *testing.T) {
	useAgentsDir(t, "Plan")
	t.Setenv(settings.AgentEnv, core.AgentGemini)
	stubLabelModels(t, "")
	label, model, _ := dispatchLabel(context.Background(), LabelInput{Type: "Plan", Model: "opus", Brief: "rename one variable"})
	if label != "opus" || model != "opus" {
		t.Fatalf("Gemini ambient agent = (%q, %q), want (opus, opus)", label, model)
	}
	if got := os.Getenv(settings.AgentEnv); got != core.AgentGemini {
		t.Errorf("ambient agent after labeling = %q, want %q", got, core.AgentGemini)
	}
}

// A Run: line's --model auto labels the concrete model it ranks, never "auto"; auto mode leaves the prompt as sent.
func TestLabelModelAutoRunLine(t *testing.T) {
	useAgentsDir(t, "claude")
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(agentconfig.OverlayVar, "")
	if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: time.Now(), Models: []router.ServedModel{
		{ID: "databricks-claude-opus-5-5", Task: "llm/v1/chat", Ready: "READY"},
		{ID: "databricks-claude-haiku-5-5", Task: "llm/v1/chat", Ready: "READY"}}}); err != nil {
		t.Fatal(err)
	}
	brief := "Run: claude-code agents ask --agent claude --profile dev --model auto --tools 'audit the safety rules'\n\nAudit."
	q, _ := labelQuery(context.Background(), newDispatchRun(nil), map[string]any{}, "general-purpose", brief)
	if q.Model != "auto" || !q.Tools || q.Task != "audit the safety rules" {
		t.Errorf("query = %+v, want auto, tools and the ask's prompt as the task", q)
	}
	raw, _ := json.Marshal(map[string]any{"tool_name": "Agent", "permission_mode": "auto",
		"tool_input": map[string]any{"subagent_type": "general-purpose", "prompt": brief, "description": "audit"}})
	r := agentEffortResult(context.Background(), core.ParseInput(bytes.NewReader(raw)))
	d, _ := r.UpdatedInput["description"].(string)
	if r.Decision == core.Block || strings.HasPrefix(d, "auto:") || !strings.HasPrefix(d, "databricks-claude-") ||
		r.UpdatedInput["prompt"] != brief {
		t.Errorf("auto-mode dispatch = %+v (%s), want a concrete model label and the prompt byte-identical", r.UpdatedInput, r.Reason)
	}
}

// A pack the agent reads from a file runs with tools on (packPrompt), so the label ranks for tools too.
func TestAskLinePackFileTools(t *testing.T) {
	if _, ok := agentpkg.LookupSchema("gemini"); !ok {
		agentpkg.RegisterRowForTest(agentpkg.Schema{Name: "gemini", CLI: agentpkg.CLI{PackVia: agentpkg.PackFile}})
	}
	for brief, want := range map[string]bool{
		"Run: claude-code agents ask --agent gemini --dirs internal 'audit x'\n\nAudit.":   true,
		"Run: claude-code agents ask --agent opencode --dirs internal 'audit x'\n\nAudit.": false,
		"Run: claude-code agents ask --agent gemini 'audit x'\n\nAudit.":                   false,
	} {
		if got := parseAskLine(context.Background(), brief).tools; got != want {
			t.Errorf("tools(%q) = %v, want %v", brief, got, want)
		}
	}
}

// A gemini dispatch on Google gets the low-pool warning and a held model's reset; no other dispatch reads agy.
func TestLabelModelGoogleQuotaNote(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reset := time.Date(2026, 11, 23, 3, 35, 47, 0, time.UTC)
	low := gemini.QuotaRow{Pool: "Gemini Models", Limit: "Five Hour Limit Remaining", Remaining: 12}
	for _, tc := range []struct {
		name, agent, account string
		q                    gemini.ModelQuota
		err                  error
		reads                int
		want                 []string
	}{
		{"low pool", "gemini", "google·antigravity", gemini.ModelQuota{Low: &low}, nil, 1, []string{"google quota low", "12% left"}},
		{"held model", "gemini", "google·antigravity", gemini.ModelQuota{HeldUntil: reset}, nil, 1,
			[]string{"is held until " + reset.Local().Format("2006-01-02 15:04"), "ask will refuse"}},
		{"read failed", "gemini", "google·antigravity", gemini.ModelQuota{}, errors.New("agy usage unavailable"), 1, nil},
		{"non-gemini dispatch", "codex", "google·antigravity", gemini.ModelQuota{Low: &low}, nil, 0, nil},
		{"non-Google route", "gemini", "databricks·dev", gemini.ModelQuota{Low: &low}, nil, 0, nil},
	} {
		reads := 0
		orig := googleQuotaCheck
		googleQuotaCheck = func(context.Context, string, time.Time) (gemini.ModelQuota, error) { reads++; return tc.q, tc.err }
		stubResolve(t, func(agentpkg.ModelQuery) string { return "gemini-4-argon-low" })
		resolveModel = func(_ *providers.Registry, q agentpkg.ModelQuery, _ time.Time) (agentpkg.ModelRoute, error) {
			return agentpkg.ModelRoute{Agent: q.Agent, Account: tc.account, Model: "gemini-4-argon-low"}, nil
		}
		task := "Run: claude-code agents ask --agent " + tc.agent + " 'do x'"
		_, note := labelModel(context.Background(), newDispatchRun(nil), map[string]any{}, "general-purpose", task)
		googleQuotaCheck = orig
		if reads != tc.reads {
			t.Errorf("%s: quota reads = %d, want %d", tc.name, reads, tc.reads)
		}
		for _, w := range tc.want {
			if !strings.Contains(note, w) {
				t.Errorf("%s: note %q lacks %q", tc.name, note, w)
			}
		}
		if tc.want == nil && strings.Contains(note, "quota") || tc.want == nil && strings.Contains(note, "held until") {
			t.Errorf("%s: note %q, want no quota line", tc.name, note)
		}
	}
}

// An equivalent label (alias, dotted or full id) is kept; a different model, or a `databricks-` claim it lacks, is not.
func TestSameLabel(t *testing.T) {
	for _, c := range []struct {
		head, label string
		want        bool
	}{
		{"sonnet", "claude-sonnet-5-5", true}, {"claude-sonnet-5.5", "claude-sonnet-5-5", true},
		{"sonnet", "databricks-claude-sonnet-5-5", true}, {"databricks-claude-sonnet-5-5", "databricks-claude-sonnet-5-5", true},
		{"databricks-claude-sonnet-5-5", "sonnet", false}, {"opus", "claude-sonnet-5-5", false}, {"", "sonnet", false},
		{"opencode-go/kimi-k3", "databricks-kimi-k3", false}, {"github/gpt-5.5", "gpt-5.5", false}, {"opencode-go/kimi-k3", "opencode-go/kimi-k3", true},
	} {
		if got := sameLabel(c.head, c.label); got != c.want {
			t.Errorf("sameLabel(%q,%q) = %v, want %v", c.head, c.label, got, c.want)
		}
	}
}

// A Databricks-routed session spells a Claude alias as its `databricks-` id; a first-party one keeps the alias.
func TestDatabricksIDOnRoutedSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	run := newDispatchRun(func() string { return "" })
	run.registry = func() (*providers.Registry, error) {
		return &providers.Registry{Providers: map[string]*providers.Provider{
			"dbx": {Preset: providers.PresetDatabricks}, "anthropic": {Provider: "anthropic"}}}, nil
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1/p/dbx")
	if got := run.databricksID("sonnet"); got != "databricks-claude-sonnet-5-5" {
		t.Errorf("databricks session: %q", got)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1/p/anthropic")
	if got := run.databricksID("sonnet"); got != "sonnet" {
		t.Errorf("first-party session: %q", got)
	}
}

// The hook accepts an equivalent hand-typed label without an updatedInput.
func TestEquivalentLabelNotRewritten(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubLabelModels(t, "claude-sonnet-5-5")
	if r := effortResult(autoDispatch("claude", "claude-sonnet-5.5: fix bug")); r.UpdatedInput != nil {
		t.Fatalf("equivalent label rewritten: %+v", r)
	}
}

// A same-model head that stripLabels cannot cut (`sonnet[1m]`) is not kept, or the label would be stacked on it.
func TestEquivalentUncuttableLabelNotStacked(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubLabelModels(t, "claude-sonnet-5-5")
	r := effortResult(autoDispatch("claude", "sonnet[1m]: fix bug"))
	if got, _ := r.UpdatedInput["description"].(string); strings.Contains(got, "sonnet[1m]: sonnet[1m]") {
		t.Fatalf("label stacked: %q", got)
	}
}
