package vendors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type ocProv struct {
	NPM     string `json:"npm"`
	Options struct {
		BaseURL string            `json:"baseURL"`
		APIKey  string            `json:"apiKey"`
		Headers map[string]string `json:"headers"`
	} `json:"options"`
	Models map[string]struct {
		Name    string `json:"name"`
		Limit   any    `json:"limit"`
		Options struct {
			ToolStreaming *bool `json:"toolStreaming"`
		} `json:"options"`
	} `json:"models"`
}

type ocCfg struct {
	Model    string            `json:"model"`
	Provider map[string]ocProv `json:"provider"`
}

func renderOpencode(t *testing.T, p *providers.Provider, dialect, base, model string, routed bool) ocCfg {
	t.Helper()
	vaulttest.Use(t)
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
		Routes: map[string]string{providers.ProtocolOpenAI: host + "/serving-endpoints", providers.ProtocolResponses: host + "/serving-endpoints"},
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
	claude, hasClaude := c.Provider["harness-"+row+"-claude"]
	total := len(chat.Models) + len(gpt.Models) + len(claude.Models)
	if total != len(providers.DatabricksOpencodeMenu) || len(gpt.Models) != 3 {
		t.Errorf("models chat=%d gpt=%d claude=%d, menu=%d", len(chat.Models), len(gpt.Models), len(claude.Models), len(providers.DatabricksOpencodeMenu))
	}
	if hasClaude && (claude.NPM != "@ai-sdk/anthropic" || claude.Options.APIKey != gpt.Options.APIKey) {
		t.Errorf("claude provider npm=%q apiKey=%q", claude.NPM, claude.Options.APIKey)
	}
	for _, m := range providers.DatabricksOpencodeMenu {
		set := chat
		switch {
		case m.Responses:
			set = gpt
		case hasClaude && models.FamilyOf(m.ID) != "":
			set = claude
		}
		e, ok := set.Models[prefix+m.ID]
		if !ok {
			t.Errorf("missing %q (responses=%v)", prefix+m.ID, m.Responses)
			continue
		}
		if (set.NPM == "@ai-sdk/anthropic") != (e.Options.ToolStreaming != nil && !*e.Options.ToolStreaming) || (e.Options.ToolStreaming != nil && set.NPM != "@ai-sdk/anthropic") {
			t.Errorf("%s toolStreaming=%v on %s", m.ID, e.Options.ToolStreaming, set.NPM)
		}
		if e.Limit != nil {
			t.Errorf("%s carries a limit: %v", m.ID, e.Limit)
		}
		if !strings.HasSuffix(e.Name, " · "+row) || !strings.HasPrefix(e.Name, m.Label) {
			t.Errorf("%s name = %q", m.ID, e.Name)
		}
	}
	for _, set := range []ocProv{chat, gpt, claude} {
		for id := range set.Models {
			if !strings.HasPrefix(id, prefix) {
				t.Errorf("id %q lacks prefix %q", id, prefix)
			}
		}
	}
}

func TestOpencodeDatabricksServing(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	c := renderOpencode(t, p, providers.ProtocolOpenAI, p.Routes[providers.ProtocolOpenAI], "databricks-claude-opus-5-5", false)
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
	g := renderOpencode(t, p, providers.ProtocolOpenAI, p.Routes[providers.ProtocolOpenAI], "databricks-gpt-6-astra", false)
	if g.Model != "harness-qa-gpt/databricks-gpt-6-astra" {
		t.Errorf("gpt default model = %q", g.Model)
	}
}

func TestOpencodeDatabricksRoutedBases(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	c := renderOpencode(t, p, providers.ProtocolOpenAI, testRouterBase+"/p/qa/openai", "databricks-claude-opus-5-5", true)
	checkDbxMenu(t, c, "qa", "databricks-")
	if got := c.Provider["harness-qa-gpt"].Options.BaseURL; got != testRouterBase+"/p/qa/responses" {
		t.Errorf("responses base = %q", got)
	}
	if got := c.Provider["harness-qa"].Options.BaseURL; got != testRouterBase+"/p/qa/openai" {
		t.Errorf("chat base = %q", got)
	}
	cl := c.Provider["harness-qa-claude"]
	if cl.Options.BaseURL != testRouterBase+"/p/qa/v1" || c.Model != "harness-qa-claude/databricks-claude-opus-5-5" {
		t.Errorf("claude base = %q model = %q", cl.Options.BaseURL, c.Model)
	}
	if _, ok := cl.Models["databricks-claude-opus-5-5"]; !ok || len(c.Provider["harness-qa"].Models)+len(cl.Models)+len(c.Provider["harness-qa-gpt"].Models) != len(providers.DatabricksOpencodeMenu) {
		t.Errorf("claude models = %v", cl.Models)
	}
}

func TestOpencodeDatabricksGatewayWire(t *testing.T) {
	p := dbxRow("dev", "system.ai.")
	c := renderOpencode(t, p, providers.ProtocolOpenAI, p.Routes[providers.ProtocolOpenAI], "system.ai.claude-opus-5-5", false)
	checkDbxMenu(t, c, "dev", "system.ai.")
	if _, ok := c.Provider["harness-dev"].Models["system.ai.kimi-k3"]; !ok {
		t.Error("system.ai.kimi-k3 missing")
	}
}

func TestOpencodeDatabricksDirectClaudeRoute(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	p.Routes[providers.ProtocolAnthropic] = "https://h.example.com/serving-endpoints/anthropic"
	c := renderOpencode(t, p, providers.ProtocolOpenAI, p.Routes[providers.ProtocolOpenAI], "databricks-claude-opus-5-5", false)
	checkDbxMenu(t, c, "qa", "databricks-")
	if got := c.Provider["harness-qa-claude"].Options.BaseURL; got != "https://h.example.com/serving-endpoints/anthropic/v1" {
		t.Errorf("direct claude base = %q", got)
	}
}

// The preset's anthropic BaseURL (no explicit route) and relative routes still resolve to absolute bases.
func TestOpencodeDatabricksPresetAndRelativeRoutes(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	p.Protocol, p.BaseURL = providers.ProtocolAnthropic, "https://h.example.com/serving-endpoints/anthropic"
	p.Routes[providers.ProtocolOpenAI] = "openai"
	c := renderOpencode(t, p, providers.ProtocolOpenAI, "https://h.example.com/serving-endpoints/anthropic/openai", "databricks-claude-opus-5-5", false)
	if got := c.Provider["harness-qa-claude"].Options.BaseURL; got != "https://h.example.com/serving-endpoints/anthropic/v1" {
		t.Errorf("preset claude base = %q", got)
	}
	if got := c.Provider["harness-qa"].Options.BaseURL; got != "https://h.example.com/serving-endpoints/anthropic/openai" {
		t.Errorf("relative chat base = %q", got)
	}
}

func TestOpencodeDatabricksUnknownModel(t *testing.T) {
	vaulttest.Use(t)
	p := dbxRow("qa", "databricks-")
	base := p.Routes[providers.ProtocolOpenAI]
	_, _, err := opencode(p, providers.ProtocolOpenAI, base, "databricks-not-a-model", "tok", false, nil)
	if err == nil || !strings.Contains(err.Error(), "databricks-not-a-model") || !strings.Contains(err.Error(), "catalog list --live") {
		t.Fatalf("unknown model err = %v", err)
	}
	p.Models = map[string]string{models.Slots[0]: "databricks-slot-only"}
	c := renderOpencode(t, p, providers.ProtocolOpenAI, base, "databricks-slot-only", false)
	if _, ok := c.Provider["harness-qa"].Models["databricks-slot-only"]; !ok || c.Model != "harness-qa/databricks-slot-only" {
		t.Errorf("slot model not accepted: %q", c.Model)
	}
	p.Models = map[string]string{models.Slots[0]: "databricks-claude-opus-9-9"}
	cc := renderOpencode(t, p, providers.ProtocolOpenAI, testRouterBase+"/p/qa/openai", "databricks-claude-opus-9-9", true)
	if e, ok := cc.Provider["harness-qa-claude"].Models["databricks-claude-opus-9-9"]; !ok || e.Options.ToolStreaming == nil || cc.Model != "harness-qa-claude/databricks-claude-opus-9-9" {
		t.Errorf("claude slot model: %q %v", cc.Model, cc.Provider["harness-qa-claude"].Models)
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

func TestOpencodeRenderUsesOverlayMenu(t *testing.T) {
	vaulttest.Use(t)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: "glm-5-2", Label: "GLM 5.2"},
		{ID: "gpt-5-5", Label: "GPT 5.5", Responses: true}}); err != nil {
		t.Fatal(err)
	}
	p := dbxRow("qa", "databricks-")
	env, _, err := opencode(p, providers.ProtocolOpenAI, p.Routes[providers.ProtocolOpenAI], "databricks-glm-5-2", "tok", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var c ocCfg
	if err := json.Unmarshal([]byte(envMap(env)["OPENCODE_CONFIG_CONTENT"]), &c); err != nil {
		t.Fatal(err)
	}
	chat, gpt := c.Provider["harness-qa"], c.Provider["harness-qa-gpt"]
	if _, ok := chat.Models["databricks-glm-5-2"]; !ok || len(chat.Models) != 1 {
		t.Errorf("chat models = %v", chat.Models)
	}
	if _, ok := gpt.Models["databricks-gpt-5-5"]; !ok || len(gpt.Models) != 1 {
		t.Errorf("gpt models = %v", gpt.Models)
	}
	for _, seed := range []string{"databricks-kimi-k3", "databricks-claude-opus-5-5", "databricks-gpt-6-1-sol"} {
		if _, ok := chat.Models[seed]; ok {
			t.Errorf("seed entry %s leaked past the overlay", seed)
		}
		if _, ok := gpt.Models[seed]; ok {
			t.Errorf("seed entry %s leaked past the overlay", seed)
		}
	}
}
