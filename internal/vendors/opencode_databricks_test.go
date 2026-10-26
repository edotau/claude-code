package vendors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

type ocProv struct {
	NPM     string `json:"npm"`
	Options struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
	} `json:"options"`
	Models map[string]struct {
		Name string `json:"name"`
	} `json:"models"`
}

type ocCfg struct {
	Model    string            `json:"model"`
	Provider map[string]ocProv `json:"provider"`
}

func renderOpencode(t *testing.T, p *providers.Provider, dialect, base, model string, routed bool) ocCfg {
	t.Helper()
	env, _, err := opencode(p, dialect, base, model, "tok", routed, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := envMap(env)["OPENCODE_CONFIG_CONTENT"]
	var c ocCfg
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("bad config json: %v\n%s", err, raw)
	}
	return c
}

func dbxRow(name, prefix string) *providers.Provider {
	host := "https://h.example.com"
	return &providers.Provider{
		Name: name, Preset: providers.PresetDatabricks, ModelPrefix: prefix,
		Routes: map[string]string{providers.DialectOpenAI: host + "/serving-endpoints", providers.DialectResponses: host + "/serving-endpoints"},
	}
}

func checkDbxMenu(t *testing.T, c ocCfg, row, prefix string) {
	t.Helper()
	chat, gpt := c.Provider["harness-"+row], c.Provider["harness-"+row+"-gpt"]
	if chat.NPM != "@ai-sdk/openai-compatible" || gpt.NPM != "@ai-sdk/openai" {
		t.Errorf("npm = %q / %q", chat.NPM, gpt.NPM)
	}
	if gpt.Options.APIKey != "{env:"+OpenCodeKeyEnv+"}" || chat.Options.APIKey != gpt.Options.APIKey {
		t.Errorf("apiKey refs = %q / %q", chat.Options.APIKey, gpt.Options.APIKey)
	}
	total := len(chat.Models) + len(gpt.Models)
	if total != len(providers.DatabricksOpencodeMenu) || len(gpt.Models) != 3 {
		t.Errorf("models chat=%d gpt=%d, menu=%d", len(chat.Models), len(gpt.Models), len(providers.DatabricksOpencodeMenu))
	}
	for _, m := range providers.DatabricksOpencodeMenu {
		set := chat
		if m.Responses {
			set = gpt
		}
		e, ok := set.Models[prefix+m.ID]
		if !ok {
			t.Errorf("missing %q (responses=%v)", prefix+m.ID, m.Responses)
			continue
		}
		if !strings.HasSuffix(e.Name, " · "+row) || !strings.HasPrefix(e.Name, m.Label) {
			t.Errorf("%s name = %q", m.ID, e.Name)
		}
	}
	for _, set := range []ocProv{chat, gpt} {
		for id := range set.Models {
			if !strings.HasPrefix(id, prefix) {
				t.Errorf("id %q lacks prefix %q", id, prefix)
			}
		}
	}
}

func TestOpencodeDatabricksServing(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	c := renderOpencode(t, p, providers.DialectOpenAI, p.Routes[providers.DialectOpenAI], "databricks-claude-opus-5-5", false)
	checkDbxMenu(t, c, "qa", "databricks-")
	if _, ok := c.Provider["harness-qa"].Models["databricks-kimi-k3"]; !ok {
		t.Error("non-claude model databricks-kimi-k3 not prefixed into the chat provider")
	}
	if c.Model != "harness-qa/databricks-claude-opus-5-5" {
		t.Errorf("model = %q", c.Model)
	}
	if got := c.Provider["harness-qa-gpt"].Options.BaseURL; got != "https://h.example.com/serving-endpoints" {
		t.Errorf("direct responses base = %q", got)
	}
	// a Responses-set target defaults into the -gpt provider
	g := renderOpencode(t, p, providers.DialectOpenAI, p.Routes[providers.DialectOpenAI], "databricks-gpt-6-astra", false)
	if g.Model != "harness-qa-gpt/databricks-gpt-6-astra" {
		t.Errorf("gpt default model = %q", g.Model)
	}
}

func TestOpencodeDatabricksRoutedBases(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	c := renderOpencode(t, p, providers.DialectOpenAI, testRouterBase+"/p/qa/openai", "databricks-claude-opus-5-5", true)
	checkDbxMenu(t, c, "qa", "databricks-")
	if got := c.Provider["harness-qa-gpt"].Options.BaseURL; got != testRouterBase+"/p/qa/responses" {
		t.Errorf("responses base = %q", got)
	}
	if got := c.Provider["harness-qa"].Options.BaseURL; got != testRouterBase+"/p/qa/openai" {
		t.Errorf("chat base = %q", got)
	}
}

func TestOpencodeDatabricksGatewayWire(t *testing.T) {
	p := dbxRow("dev", "system.ai.")
	c := renderOpencode(t, p, providers.DialectOpenAI, p.Routes[providers.DialectOpenAI], "system.ai.claude-opus-5-5", false)
	checkDbxMenu(t, c, "dev", "system.ai.")
	if _, ok := c.Provider["harness-dev"].Models["system.ai.kimi-k3"]; !ok {
		t.Error("system.ai.kimi-k3 missing")
	}
}

func TestOpencodeNonDatabricksSingleProvider(t *testing.T) {
	ctx := WithDirect(t.Context())
	env, _ := cfgFor(t, ctx, "opencode", "openai", "gpt-5")
	var c ocCfg
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Provider) != 1 {
		t.Fatalf("providers = %d, want 1: %v", len(c.Provider), c.Provider)
	}
	prov, ok := c.Provider["harness-openai"]
	if !ok || strings.Contains(c.Model, "-gpt") || c.Model != "harness-openai/gpt-5" {
		t.Errorf("model %q provider keys %v", c.Model, c.Provider)
	}
	if _, ok := prov.Models["gpt-5"]; !ok || strings.Contains(envMap(env)["OPENCODE_CONFIG_CONTENT"], "·") {
		t.Errorf("unexpected menu: %v", prov.Models)
	}
}
