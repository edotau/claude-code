package agent

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A property named "properties" is data, not a schema: only real object schemas get required/additionalProperties.
func TestCodexStrictWalksSchemaKeywordsOnly(t *testing.T) {
	var v any
	src := `{"type":"object","properties":{"properties":{"type":"string"},"items":{"type":"array",` +
		`"items":{"type":"object","properties":{"n":{"type":"integer"}}}}},"$defs":{"d":{"properties":{"z":{}}}}}`
	if err := json.Unmarshal([]byte(src), &v); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(codexStrict(v))
	want := `{"$defs":{"d":{"additionalProperties":false,"properties":{"z":{}},"required":["z"]}},` +
		`"additionalProperties":false,"properties":{"items":{"items":{"additionalProperties":false,` +
		`"properties":{"n":{"type":"integer"}},"required":["n"],"type":"object"},"type":"array"},` +
		`"properties":{"type":"string"}},"required":["items","properties"],"type":"object"}`
	if string(got) != want {
		t.Fatalf("codexStrict =\n%s\nwant\n%s", got, want)
	}
}

// Regression: an explicit GPT model (Astra) must reach codex argv, not be swapped for the first Responses entry (Sol).
func TestCodexKeepsExplicitGPTModel(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "gpt-6-1-sol", Responses: true}, {ID: "gpt-6-astra", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		p     *providers.Provider
		model string
	}{
		{"databricks astra", dbxRow("dev", "databricks-"), "databricks-gpt-6-astra"},
		{"non-databricks astra", &providers.Provider{Name: "other"}, "gpt-6-astra"},
	}
	for _, c := range cases {
		_, argv, err := codex(c.p, "http://127.0.0.1/x", c.model, "", &Headless{Effort: "high"})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := "model=" + tomlQuote(c.model); !slices.Contains(argv, want) {
			t.Errorf("%s: argv %q lacks %s", c.name, argv, want)
		}
		for _, sol := range []string{"gpt-6-1-sol", "databricks-gpt-6-1-sol"} {
			if slices.Contains(argv, "model="+tomlQuote(sol)) {
				t.Errorf("%s: argv %q routed to %s", c.name, argv, sol)
			}
		}
		if want := "model_reasoning_effort=" + tomlQuote("high"); !slices.Contains(argv, want) {
			t.Errorf("%s: argv %q lacks %s", c.name, argv, want)
		}
	}
}

// Regression (decision 24e7c144): a chat-only gpt-* model (gpt-oss) is never posted to /responses; codex swaps it.
func TestCodexChatOnlyGPTSwapped(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "gpt-oss-120b"}, {ID: "gpt-6-1-sol", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{
		"databricks-gpt-oss-120b": "databricks-gpt-6-1-sol",
		"databricks-gpt-6-1-sol":  "databricks-gpt-6-1-sol",
	} {
		_, argv, err := codex(dbxRow("dev", "databricks-"), "http://127.0.0.1/x", model, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if w := "model=" + tomlQuote(want); !slices.Contains(argv, w) {
			t.Errorf("%s: argv %q lacks %s", model, argv, w)
		}
	}
}

// Regression: ask reported the requested model although codex's Responses swap ran another one.
func TestCodexRanModelNamesTheSwappedModel(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "gpt-6-1-sol", Responses: true}, {ID: "gpt-6-astra", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	p := dbxRow("dev", "databricks-")
	for _, c := range []struct{ name, model, want string }{
		{"non-Responses model is swapped", "databricks-claude-opus-x", "databricks-gpt-6-1-sol"},
		{"Responses model is kept", "databricks-gpt-6-astra", "databricks-gpt-6-astra"},
	} {
		if got := codexRanModel(providers.Target{Provider: p, Model: c.model}); got != c.want {
			t.Errorf("%s: CodexRanModel(%q) = %q, want %q", c.name, c.model, got, c.want)
		}
	}
	if got := codexRanModel(providers.Target{Model: "m"}); got != "m" {
		t.Errorf("nil provider: got %q, want m", got)
	}
}

// Regression: codex on a Databricks row was handed the opus slot, which Databricks rejects on Responses.
func TestCodexResponsesModel(t *testing.T) {
	vaulttest.Use(t)
	menu := []providers.OpencodeModel{{ID: "claude-opus-5-5"}, {ID: "gpt-6-1-sol", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		p     *providers.Provider
		model string
		want  string
	}{
		{"databricks claude slot", dbxRow("dev", "databricks-"), "databricks-claude-opus-5-5", "databricks-gpt-6-1-sol"},
		{"databricks gpt kept", dbxRow("dev", "databricks-"), "databricks-gpt-6-1-sol", "databricks-gpt-6-1-sol"},
		{"non-databricks kept", &providers.Provider{Name: "other"}, "claude-opus-5-5", "claude-opus-5-5"},
	}
	for _, c := range cases {
		_, argv, err := codex(c.p, "http://127.0.0.1/x", c.model, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if want := "model=" + tomlQuote(c.want); !slices.Contains(argv, want) {
			t.Errorf("%s: argv %q lacks %s", c.name, argv, want)
		}
	}
}
