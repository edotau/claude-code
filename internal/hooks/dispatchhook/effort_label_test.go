package dispatchhook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/driver/gemini"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/served"
)

// A --profile row spells the model as it serves it; a typed `<row>:<model>` gets no label.
func TestLabelModelSpellsExplicitModelOnProfileRow(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(route.OverlayVar, "")
	run := newDispatchRun(nil)
	run.profile = "dev"
	if got, _ := labelModel(context.Background(), run, map[string]any{"model": "claude-opus-5"}, "general-purpose", "do x"); got != "databricks-claude-opus-5" {
		t.Errorf("label = %q, want databricks-claude-opus-5", got)
	}
	if got, note := labelModel(context.Background(), newDispatchRun(nil), map[string]any{"model": "dev:claude-opus-5"}, "general-purpose", "do x"); got != "" ||
		!strings.Contains(note, `--overlay '{"provider":"databricks","profile":"dev"}' --model claude-opus-5`) {
		t.Errorf("prefixed label = %q, note %q; want none, naming the overlay", got, note)
	}
}

// A dispatch's exact --model reaches the resolver as typed, with no default tier.
func TestLabelModelExactDispatchModel(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(route.OverlayVar, "")
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
	t.Setenv(route.OverlayVar, "")
	if err := served.Save(served.Served{Profile: "dev", FetchedAt: time.Now(), Models: []served.ServedModel{
		{ID: "databricks-glm-5-2", Ready: "READY"}, {ID: "databricks-custom-model", Ready: "READY"},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"glm-5-2", "custom-model"} {
		run := newDispatchRun(nil)
		run.profile = "dev"
		if got, _ := labelModel(context.Background(), run, map[string]any{"model": model}, "general-purpose", "do x"); got != "databricks-"+model {
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

// Every dispatch is labelled with the bare id, a retired (<id>) head included; a prefixed model gets none.
func TestAutoRunLineLabelsNext(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	stubLabelModels(t, "m-1")
	run := func(model, desc string) string {
		u := map[string]any{"subagent_type": "opencode", "description": desc, "model": model}
		applyLabel(context.Background(), newDispatchRun(nil), u, "x")
		d, _ := u["description"].(string)
		return d
	}
	for _, c := range []struct{ model, desc, want string }{
		{"auto", "do x", "m-1: do x"}, {"auto", "m-1: do x", "m-1: do x"}, {"auto", "(m-1): do x", "m-1: do x"},
		{"auto", "auto: do x", "m-1: do x"}, {"auto", "auto (next: m-1): do x", "m-1: do x"},
		{"auto", "(wip): fix x", "m-1: (wip): fix x"}, {"auto", "auto (retry) job: fix x", "m-1: auto (retry) job: fix x"},
		{"m-1", "(m-1): do x", "m-1: do x"}, {"m-1", "m-1: do x", "m-1: do x"}, {"m-1", "(wip): fix x", "m-1: (wip): fix x"},
		{"dev:auto", "do x", "do x"}, {"m-2", "(m-0): do x", "m-1: do x"}, // a prefixed model gets no label
	} {
		if got := run(c.model, c.desc); got != c.want {
			t.Errorf("--model %s, %q -> %q, want %q", c.model, c.desc, got, c.want)
		}
	}
}

// Bug 2 (2026-11-19): the rewrite note quotes the full old head and new label, never a shared-suffix rune slice.
func TestApplyLabelNoteQuotesHeadsExactly(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, label := range []string{"databricks-claude-opus-5-5", "databricks-claude-opus-5", "gpt-6.1-sol", "gemini-4-hydrogen"} {
		stubLabelModels(t, label)
		updated := map[string]any{"subagent_type": "opencode", "model": "fixed-1", "description": "databricks-claude-opus-5-5: audit"}
		changed, note := applyLabel(context.Background(), newDispatchRun(nil), updated, "do x")
		if label == "databricks-claude-opus-5-5" { // the equivalent label is kept: no rewrite, no head note
			if changed || strings.Contains(note, "rewritten to") {
				t.Errorf("equivalent label: changed=%v note %q, want no rewrite", changed, note)
			}
			continue
		}
		want := fmt.Sprintf("description head %q rewritten to %q.", "databricks-claude-opus-5-5: ", label+": ")
		if !changed || updated["description"] != label+": audit" || !strings.Contains(note, want) {
			t.Errorf("label %q: changed=%v desc=%q note %q, want %q", label, changed, updated["description"], note, want)
		}
	}
}

// A gemini dispatch on Google gets the low-pool warning and a held model's reset; no other dispatch reads agy.
func TestLabelModelGoogleQuotaNote(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reset := time.Now().Add(time.Hour) // a fixed date expires: a past hold prints no note
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
		resolveModel = func(_ *providers.Registry, q agentpkg.ModelQuery, _ time.Time) (agentpkg.ModelRoute, *agentpkg.AutoChoice, error) {
			return agentpkg.ModelRoute{Agent: q.Agent, Account: tc.account, Model: "gemini-4-argon-low"}, nil, nil
		}
		_, note := labelModel(context.Background(), newDispatchRun(nil), map[string]any{}, tc.agent, "do x")
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

// With no resolved model a typed model head is dropped, not kept as a claim; a Fable label on Databricks is flagged.
func TestLabelNamesOnlyTheModelThatRuns(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(route.OverlayVar, "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	u := map[string]any{"subagent_type": "claude", "description": "databricks-kimi-k3: do x"}
	if changed, _ := applyLabel(context.Background(), newDispatchRun(func() string { return "" }), u, "do x"); !changed || u["description"] != "do x" {
		t.Errorf("unresolved label: changed=%v description %q, want the stale head dropped", changed, u["description"])
	}
	run := newDispatchRun(nil)
	run.profile = "dev"
	if n := run.fableNote(agentpkg.ModelRoute{}, "databricks-claude-fable-5-2"); !strings.Contains(n, "first-party Anthropic") {
		t.Errorf("fableNote on dev = %q, want the Anthropic route named", n)
	}
	vaulttest.SetRecord(t, "claude", "model", "databricks-claude-sonnet-5-5")
	if n := run.fableNote(agentpkg.ModelRoute{}, "databricks-claude-fable-5-2"); strings.Contains(n, "sonnet") {
		t.Errorf("fableNote with a Sonnet all-slot pin = %q, want no Sonnet id", n)
	}
	if n := run.fableNote(agentpkg.ModelRoute{}, "databricks-claude-opus-5-5"); n != "" {
		t.Errorf("fableNote for Opus = %q, want none", n)
	}
	run.profile = "anthropic"
	if n := run.fableNote(agentpkg.ModelRoute{}, "claude-fable-5-2"); n != "" {
		t.Errorf("fableNote on a first-party row = %q, want none", n)
	}
}

// An equivalent label (alias, dotted or full id) is kept; a different model, or a `databricks-` claim it lacks, is not.
func TestSameLabel(t *testing.T) {
	for _, c := range []struct {
		head, label string
		want        bool
	}{
		{"sonnet", "claude-sonnet-5-5", true}, {"sonnet", "sonnet", true}, {"gemini", "databricks-gemini-4-argon", false},
		{"codex", "gpt-6.1-sol", false}, {"gpt", "gpt-6.1-sol", false}, {"databricks-gemini-4-argon", "databricks-gemini-4-argon", true},
		{"claude-sonnet-5.5", "claude-sonnet-5-5", true},
		{"sonnet", "databricks-claude-sonnet-5-5", true}, {"databricks-claude-sonnet-5-5", "databricks-claude-sonnet-5-5", true},
		{"databricks-claude-sonnet-5-5", "sonnet", false}, {"opus", "claude-sonnet-5-5", false}, {"", "sonnet", false},
		{"opencode-go/kimi-k3", "databricks-kimi-k3", false}, {"github/gpt-5.5", "gpt-5.5", false}, {"opencode-go/kimi-k3", "opencode-go/kimi-k3", true},
	} {
		if got := sameLabel(c.head, c.label); got != c.want {
			t.Errorf("sameLabel(%q,%q) = %v, want %v", c.head, c.label, got, c.want)
		}
	}
}

// A bare family or agent head is rewritten to the full label; the label itself is kept byte-identical.
func TestApplyLabelRewritesBareHead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, c := range []struct{ label, desc, want string }{
		{"databricks-gemini-4-argon", "gemini: x", "databricks-gemini-4-argon: x"},
		{"databricks-gemini-4-argon", "codex: x", "databricks-gemini-4-argon: x"},
		{"gpt-6.1-sol", "gpt: x", "gpt-6.1-sol: x"},
		{"databricks-gemini-4-argon", "databricks-gemini-4-argon: x", "databricks-gemini-4-argon: x"},
		{"sonnet", "sonnet: x", "sonnet: x"},
	} {
		stubLabelModels(t, c.label)
		u := map[string]any{"subagent_type": "opencode", "model": "fixed-1", "description": c.desc}
		applyLabel(context.Background(), newDispatchRun(nil), u, "x")
		if got := u["description"]; got != c.want {
			t.Errorf("label %q, %q -> %q, want %q", c.label, c.desc, got, c.want)
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

func TestLabelQueryTypedAliasIsTheAsksModel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tc := range []struct{ typ, model, want string }{
		{"opencode", "opus", "opus"}, // the verb's --model alias beats the pin
		{"opencode", "", ""},
		{"opencode", "inherit", ""},
		{"claude", "opus", ""}, // an in-context leaf keeps its own model
	} {
		run := newDispatchRun(nil)
		run.model = tc.model
		q, _ := labelQuery(run, map[string]any{"model": tc.model}, tc.typ, "do x")
		if q.Model != tc.want {
			t.Errorf("%s --model %q: query model %q, want %q", tc.typ, tc.model, q.Model, tc.want)
		}
	}
}

// An inherit agent on a Databricks route labels Opus for a Fable session id; a typed Fable stays refused.
func TestInheritedFableSessionLabelsOpusOnDatabricks(t *testing.T) {
	useAgentsDir(t, "Plan:inherit")
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(route.OverlayVar, "")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:18765/p/dev")
	run := newDispatchRun(func() string { return "databricks-claude-fable-5-2" })
	if got := ownModel(context.Background(), run, map[string]any{}, "Plan"); got != "databricks-claude-opus-5-5" {
		t.Errorf("inherit Plan on a Fable session = %q, want databricks-claude-opus-5-5", got)
	}
	run.profile = "dev"
	if n := run.fableNote(agentpkg.ModelRoute{}, "databricks-claude-fable-5-2"); !strings.Contains(n, "first-party Anthropic") {
		t.Errorf("a typed Fable label lost its route note: %q", n)
	}
}

// An agent file baking --provider databricks labels through that provider, not the agent's own pin.
func TestLabelQueryUsesBakedProvider(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", armDevOverlay)
	t.Setenv(route.OverlayVar, "")
	old := readAgentFile
	readAgentFile = func(string) ([]byte, error) {
		return []byte("```bash\nclaude-code agents ask --agent codex --provider databricks --profile dev --tools - < PATH\n```\n"), nil
	}
	t.Cleanup(func() { readAgentFile = old })
	var seen agentpkg.ModelQuery
	stubResolve(t, func(q agentpkg.ModelQuery) string { seen = q; return "wire-id" })
	labelModel(context.Background(), newDispatchRun(nil), map[string]any{}, "codex", "do x")
	if seen.Provider != "databricks" {
		t.Errorf("query provider = %q, want databricks from the baked ask line", seen.Provider)
	}
}

// An auto route's label is the peeked pick, else the literal auto, never the static pin.
func TestLabelModelAutoIsPickNotPin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tc := range []struct {
		name   string
		choice *agentpkg.AutoChoice
		want   string
	}{
		{"peeked pick", &agentpkg.AutoChoice{Criterion: "rotation"}, "picked-id"},
		{"cold fallback", &agentpkg.AutoChoice{Fallback: "no fresh listing"}, "auto"},
	} {
		stubResolve(t, func(agentpkg.ModelQuery) string { return "picked-id" })
		resolveModel = func(_ *providers.Registry, q agentpkg.ModelQuery, _ time.Time) (agentpkg.ModelRoute, *agentpkg.AutoChoice, error) {
			return agentpkg.ModelRoute{Agent: q.Agent, Model: "static-pin"}, tc.choice, nil
		}
		if tc.choice != nil && tc.choice.Fallback == "" {
			resolveModel = func(_ *providers.Registry, q agentpkg.ModelQuery, _ time.Time) (agentpkg.ModelRoute, *agentpkg.AutoChoice, error) {
				return agentpkg.ModelRoute{Agent: q.Agent, Model: "picked-id"}, tc.choice, nil
			}
		}
		run := newDispatchRun(nil)
		run.model = agentpkg.AutoModel
		if got, _ := labelModel(context.Background(), run, map[string]any{}, "opencode", "do x"); got != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.name, got, tc.want)
		}
	}
}
