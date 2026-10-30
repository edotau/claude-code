package vendors

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// gwRow is a Databricks ai-gateway row: anthropic + responses routes, no openai route.
func gwRow(name string) *providers.Provider {
	h := "https://h.example.com"
	return &providers.Provider{
		Name: name, Preset: providers.PresetDatabricks, ModelPrefix: "system.ai.",
		Protocol: providers.ProtocolAnthropic, BaseURL: h + "/ai-gateway/anthropic",
		Routes: map[string]string{providers.ProtocolResponses: h + "/ai-gateway/codex/v1"},
	}
}

// CanServe is Serves without the reason, over every roster agent and a spread of rows; gemini on Databricks
// is the one exception (the router's gemini route, not a row dialect).
func TestCanServeEqualsServesOK(t *testing.T) {
	rows := []*providers.Provider{
		dbxRow("qa", "databricks-"), gwRow("gw"),
		{Name: "ant", Protocol: providers.ProtocolAnthropic, BaseURL: "https://a", Auth: providers.Auth{Type: providers.AuthKey}},
		{Name: "sub", Protocol: providers.ProtocolAnthropic, BaseURL: "https://s", Auth: providers.Auth{Type: providers.AuthCLI}},
		{Name: "gh", Protocol: providers.ProtocolOpenAI, BaseURL: "https://g", Auth: providers.Auth{Type: providers.AuthCLI, Login: "copilot"}},
		{Name: "gem", Routes: map[string]string{providers.ProtocolGemini: "https://g"}},
	}
	for _, s := range providers.AgentSpecs() {
		if s.Binary == "" {
			continue
		}
		for _, p := range rows {
			if s.Name == "gemini" && providers.GeminiGatewayRow(p) {
				if !providers.CanServe(s.Name, p) {
					t.Errorf("CanServe(gemini,%s) = false on a Databricks row", p.Name)
				}
				continue
			}
			if ok, _ := providers.Serves(s.Name, p, ""); providers.CanServe(s.Name, p) != ok {
				t.Errorf("CanServe(%s,%s) = %v, Serves = %v", s.Name, p.Name, providers.CanServe(s.Name, p), ok)
			}
		}
	}
}

// configureRoute surfaces Serves' reason verbatim, prefixed by the agent.
func TestConfigureRouteUsesServesReason(t *testing.T) {
	vaulttest.Use(t)
	cases := []struct {
		agent string
		p     *providers.Provider
	}{
		{"gemini", &providers.Provider{Name: "ant", Protocol: providers.ProtocolAnthropic, BaseURL: "https://a", Auth: providers.Auth{Type: providers.AuthKey}}},
		{"codex", &providers.Provider{Name: "ant", Protocol: providers.ProtocolAnthropic, BaseURL: "https://a", Auth: providers.Auth{Type: providers.AuthKey}}},
		{"copilot", &providers.Provider{Name: "sub", Protocol: providers.ProtocolAnthropic, BaseURL: "https://s", Auth: providers.Auth{Type: providers.AuthCLI}}},
	}
	for _, c := range cases {
		_, why := providers.Serves(c.agent, c.p, "")
		if why == "" {
			t.Fatalf("%s/%s: control: Serves has no reason", c.agent, c.p.Name)
		}
		_, _, err := configureRoute(t.Context(), c.agent, providers.Target{Provider: c.p, Model: "m"}, nil)
		if err == nil || err.Error() != c.agent+": "+why {
			t.Errorf("%s/%s: err = %v, want %q", c.agent, c.p.Name, err, c.agent+": "+why)
		}
	}
}

// D5: an ai-gateway row has no openai route, so the opencode menu has no chat provider; Claude and GPT stay.
func TestOpencodeDatabricksGatewayDropsChatProvider(t *testing.T) {
	var warn strings.Builder
	old := catalogWarn
	catalogWarn = &warn
	t.Cleanup(func() { catalogWarn = old })
	p := gwRow("gw")
	c := renderOpencode(t, p, providers.ProtocolAnthropic, p.BaseURL, "system.ai.claude-opus-5-5", false)
	if _, ok := c.Provider["harness-gw"]; ok {
		t.Error("chat provider emitted on a row without an openai route")
	}
	if gpt := c.Provider["harness-gw-gpt"]; len(gpt.Models) != 3 || gpt.NPM != "@ai-sdk/openai" {
		t.Errorf("gpt provider = %+v", gpt)
	}
	cl := c.Provider["harness-gw-claude"]
	if _, ok := cl.Models["system.ai.claude-opus-5-5"]; !ok || cl.NPM != "@ai-sdk/anthropic" || c.Model != "harness-gw-claude/system.ai.claude-opus-5-5" {
		t.Errorf("claude provider = %+v model %q", cl, c.Model)
	}
	if !strings.Contains(warn.String(), "gw has no openai route") || !strings.Contains(warn.String(), "Kimi K3") {
		t.Errorf("warn = %q", warn.String())
	}
	_, _, err := opencode(p, providers.ProtocolAnthropic, p.BaseURL, "system.ai.kimi-k3", "tok", false, nil)
	if err == nil || !strings.Contains(err.Error(), "no openai route") {
		t.Errorf("chat model on a gateway row: err = %v", err)
	}
}

// A serving row keeps its chat provider and warns of nothing.
func TestOpencodeDatabricksServingKeepsChatProvider(t *testing.T) {
	var warn strings.Builder
	old := catalogWarn
	catalogWarn = &warn
	t.Cleanup(func() { catalogWarn = old })
	p := dbxRow("qa", "databricks-")
	c := renderOpencode(t, p, providers.ProtocolOpenAI, p.Routes[providers.ProtocolOpenAI], "databricks-claude-opus-5-5", false)
	if len(c.Provider["harness-qa"].Models) == 0 || warn.Len() != 0 {
		t.Errorf("chat models %d, warn %q", len(c.Provider["harness-qa"].Models), warn.String())
	}
}

// A row with no responses route cannot render the menu.
func TestOpencodeDatabricksNoResponsesRouteRefused(t *testing.T) {
	vaulttest.Use(t)
	p := dbxRow("qa", "databricks-")
	delete(p.Routes, providers.ProtocolResponses)
	_, _, err := opencode(p, providers.ProtocolOpenAI, p.Routes[providers.ProtocolOpenAI], "databricks-kimi-k3", "tok", false, nil)
	if err == nil || !strings.Contains(err.Error(), "lacks a responses route") {
		t.Errorf("err = %v", err)
	}
}

// D8: the registry launch needs a keyed, non-login, non-direct Databricks row.
func TestCopilotRegistryEligible(t *testing.T) {
	row := func(mut func(*providers.Provider)) providers.Target {
		p := dbxRow("qa", "databricks-")
		p.Auth = providers.Auth{Type: providers.AuthOAuth}
		if mut != nil {
			mut(p)
		}
		return providers.Target{Provider: p}
	}
	cases := []struct {
		name   string
		direct bool
		t      providers.Target
		want   bool
	}{
		{"keyed databricks", false, row(nil), true},
		{"direct", true, row(nil), false},
		{"auth none", false, row(func(p *providers.Provider) { p.Auth = providers.Auth{Type: providers.AuthNone} }), false},
		{"auth cli", false, row(func(p *providers.Provider) { p.Auth = providers.Auth{Type: providers.AuthCLI} }), false},
		{"login", false, row(func(p *providers.Provider) { p.Auth = providers.Auth{Type: providers.AuthCLI, Login: "copilot"} }), false},
		{"not databricks", false, row(func(p *providers.Provider) { p.Preset = "" }), false},
		{"nil provider", false, providers.Target{}, false},
	}
	for _, c := range cases {
		if got := CopilotRegistryEligible(c.direct, c.t); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", c.name, got, c.want)
		}
	}
	ctx := WithDirect(t.Context())
	if UsesCopilotRegistry(ctx, row(nil)) || !UsesCopilotRegistry(t.Context(), row(nil)) {
		t.Error("UsesCopilotRegistry must follow CopilotRegistryEligible")
	}
}
