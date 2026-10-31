package agent

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func specTarget(kind string) providers.Target {
	switch kind {
	case "databricks":
		p := dbxRow("dev", "databricks-")
		p.Models = map[string]string{"opus": "databricks-claude-opus-5-5", "fable": "databricks-claude-fable-5-1"}
		return providers.Target{Provider: p, Model: "databricks-base"}
	case "own-login":
		return providers.Target{Provider: &providers.Provider{Name: "chatgpt", Auth: providers.Auth{Login: "codex"}}, Model: "own-base"}
	}
	p := &providers.Provider{Name: "anthropic", Models: map[string]string{"opus": "claude-opus-5-5"}}
	return providers.Target{Provider: p, Model: "fallback-base"}
}

func TestResolveAgentModel(t *testing.T) {
	hasModel := map[string]bool{"codex": true, "copilot": true, "gemini": true}
	for _, agent := range []string{"codex", "copilot", "gemini", "opencode"} {
		for _, kind := range []string{"databricks", "own-login", "fallback"} {
			for _, mode := range []string{"flag", "pin", "none"} {
				t.Run(agent+"/"+kind+"/"+mode, func(t *testing.T) {
					vaulttest.Use(t)
					if hasModel[agent] && mode != "none" {
						vaulttest.SetRecord(t, agent, "model", "pinned-model")
					}
					tg := specTarget(kind)
					ask := ModelAsk{Agent: agent, Target: tg}
					want := tg.Model
					switch mode {
					case "flag":
						ask.Flag, want = "flag-model", "flag-model"
					case "pin":
						if hasModel[agent] {
							want = "pinned-model"
						}
					}
					got, note := ResolveAgentModel(ask)
					if got.Model != want || note != "" || got.Provider != tg.Provider {
						t.Errorf("got %q (note %q), want %q", got.Model, note, want)
					}
				})
			}
		}
	}
}

// D1: the CODEX_MODEL pin reaches a codex run on a Databricks session target, spelled the way Databricks serves it.
func TestResolveAgentModelCodexPinOnDatabricks(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "codex", "model", "databricks-gpt-6-1-sol")
	got, _ := ResolveAgentModel(ModelAsk{Agent: "codex", Target: specTarget("databricks")})
	if got.Model != "databricks-gpt-6-1-sol" {
		t.Errorf("model = %q, want the pin", got.Model)
	}
	if got, _ = ResolveAgentModel(ModelAsk{Agent: "codex", Flag: "gpt-5.5", Target: specTarget("databricks")}); got.Model != "databricks-gpt-5-5" {
		t.Errorf("flag = %q, want it respelled for Databricks and beating the pin", got.Model)
	}
}

// D7: both copilot registry paths take the model pin through CopilotChoice; a requested model rides alongside it.
func TestCopilotChoicePin(t *testing.T) {
	vaulttest.Use(t)
	if c := copilotChoice(nil); c.Pin != "" {
		t.Fatalf("no pin set: %+v", c)
	}
	vaulttest.SetRecord(t, "copilot", "model", "kimi-k3")
	c := copilotChoice([]string{"a", "b"})
	if c.Pin != "kimi-k3" || strings.Join(c.Requested, ",") != "a,b" {
		t.Errorf("choice = %+v", c)
	}
}

// D10: a slot alias the provider lacks keeps the default model and says so; one it has maps through.
func TestResolveAgentModelMissingAlias(t *testing.T) {
	vaulttest.Use(t)
	tg := specTarget("fallback")
	got, note := ResolveAgentModel(ModelAsk{Agent: "copilot", Flag: "fable", Target: tg})
	if got.Model != "fallback-base" || note != "anthropic has no fable model; keeping fallback-base" {
		t.Errorf("got %q / %q", got.Model, note)
	}
	got, note = ResolveAgentModel(ModelAsk{Agent: "copilot", Flag: "opus", Target: tg})
	if got.Model != "claude-opus-5-5" || note != "" {
		t.Errorf("opus = %q / %q", got.Model, note)
	}
	if got, note = ResolveAgentModel(ModelAsk{Agent: "copilot", Flag: "fable", Target: tg, Applied: true}); got.Model != "fallback-base" || note != "" {
		t.Errorf("applied flag must be left alone: %q / %q", got.Model, note)
	}
}

// A databricks- pin is dropped off Databricks (the default stands) and kept on it.
func TestSpellPinDropsDatabricksOffDatabricks(t *testing.T) {
	const pin = "databricks-gpt-6-1-sol"
	if got := providers.SpellPin(specTarget("fallback").Provider, pin); got != "" {
		t.Errorf("off-Databricks = %q, want dropped", got)
	}
	if got := providers.SpellPin(specTarget("databricks").Provider, pin); got != pin {
		t.Errorf("Databricks = %q, want kept", got)
	}
	vaulttest.Use(t)
	vaulttest.SetRecord(t, "codex", "model", pin)
	if got, _ := ResolveAgentModel(ModelAsk{Agent: "codex", Target: specTarget("fallback")}); got.Model != "fallback-base" {
		t.Errorf("model = %q, want the default", got.Model)
	}
}

// D4: gemini (no Dialects) is refused on any non-login row, even one with a gemini route.
func TestGeminiRefusedOnRoutedRow(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "gem-key", Models: map[string]string{"opus": "gemini-x"},
		Routes: map[string]string{providers.ProtocolGemini: "https://g.example.com"}}
	if CanServe("gemini", p) {
		t.Error("CanServe(gemini) = true on a keyed row with a gemini route")
	}
	if !CanServe("opencode", p) {
		t.Error("control: opencode speaks gemini and should serve the row")
	}
	_, _, err := schemaFor("gemini").configureRoute(t.Context(), providers.Target{Provider: p, Model: "gemini-x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "agy has no custom base URL") {
		t.Errorf("err = %v, want the agy no-custom-base-URL refusal", err)
	}
}
