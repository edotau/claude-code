package agent

import (
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
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
	for _, s := range Roster() {
		if !s.External() {
			continue
		}
		for _, p := range rows {
			if s.Name == "gemini" && GeminiGatewayRow(p) {
				if !CanServe(s.Name, p) {
					t.Errorf("CanServe(gemini,%s) = false on a Databricks row", p.Name)
				}
				continue
			}
			if ok, _ := Serves(s.Name, p, ""); CanServe(s.Name, p) != ok {
				t.Errorf("CanServe(%s,%s) = %v, Serves = %v", s.Name, p.Name, CanServe(s.Name, p), ok)
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
		_, why := Serves(c.agent, c.p, "")
		if why == "" {
			t.Fatalf("%s/%s: control: Serves has no reason", c.agent, c.p.Name)
		}
		_, _, err := schemaFor(c.agent).configureRoute(t.Context(), providers.Target{Provider: c.p, Model: "m"}, nil)
		if err == nil || err.Error() != c.agent+": "+why {
			t.Errorf("%s/%s: err = %v, want %q", c.agent, c.p.Name, err, c.agent+": "+why)
		}
	}
}

func captureCatalogWarn(t *testing.T) *strings.Builder {
	t.Helper()
	var warn strings.Builder
	old := CatalogWarn
	CatalogWarn = &warn
	t.Cleanup(func() { CatalogWarn = old })
	return &warn
}

// D5: an ai-gateway row has no openai route, so the opencode menu has no chat provider; Claude and GPT stay.
func TestOpencodeDatabricksGatewayDropsChatProvider(t *testing.T) {
	warn := captureCatalogWarn(t)
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
}

// A chat model on an ai-gateway row is refused: there is no openai route to send it to.
func TestOpencodeDatabricksGatewayRefusesChatModel(t *testing.T) {
	captureCatalogWarn(t)
	p := gwRow("gw")
	_, _, err := opencode(p, providers.ProtocolAnthropic, p.BaseURL, "system.ai.kimi-k3", "tok", false, nil)
	if err == nil || !strings.Contains(err.Error(), "no openai route") {
		t.Errorf("chat model on a gateway row: err = %v", err)
	}
}

// A serving row keeps its chat provider and warns of nothing.
func TestOpencodeDatabricksServingKeepsChatProvider(t *testing.T) {
	warn := captureCatalogWarn(t)
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

// The copilot fallback keeps the registry's declared Databricks profile order.
func TestOrderedDatabricksKeepsRegistryOrder(t *testing.T) {
	reg, err := providers.Parse([]byte(`{"providers":{"databricks":{
		"prod":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
		"zeta":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
		"dev":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
		"qa":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
		"alpha":{"preset":"databricks","host":"https://h.cloud.databricks.com"}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	reg.Providers["max"] = &providers.Provider{Name: "max", Provider: "anthropic"}
	want := []string{"prod", "zeta", "dev", "qa", "alpha"}
	if got := providers.DatabricksProfiles(reg); !slices.Equal(got, want) {
		t.Errorf("DatabricksProfiles = %v, want %v", got, want)
	}
}

// dbxServing points a Databricks serving row's routes at srv (anthropic at /anthropic, openai/responses at /v1).
func dbxServing(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("TEST_DBX_KEY", "dbx-key")
	body := fmt.Sprintf(`{"providers":{"databricks":{"dbx":{"preset":"databricks","host":"https://h.cloud.databricks.com",
	  "base_url":%q,"routes":{"openai":%q,"responses":%q},"auth":{"type":"key","env":"TEST_DBX_KEY"}}}}}`,
		srv.URL+"/anthropic", srv.URL+"/v1", srv.URL+"/v1")
	vaulttest.Put(t, vault.Overlay, "providers", body)
}
