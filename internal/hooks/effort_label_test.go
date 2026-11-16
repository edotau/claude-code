package hooks

import (
	"context"
	"os"
	"testing"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
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
