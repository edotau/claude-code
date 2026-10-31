package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

func captureCatalogWarn(t *testing.T) *strings.Builder {
	t.Helper()
	var warn strings.Builder
	old := catalogWarn
	catalogWarn = &warn
	t.Cleanup(func() { catalogWarn = old })
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
		if got := copilotRegistryEligible(c.direct, c.t); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", c.name, got, c.want)
		}
	}
	ctx := WithDirect(t.Context())
	if UsesCopilotRegistry(ctx, row(nil)) || !UsesCopilotRegistry(t.Context(), row(nil)) {
		t.Error("UsesCopilotRegistry must follow CopilotRegistryEligible")
	}
}

func TestMenuWire(t *testing.T) {
	p := dbxRow("qa", "databricks-")
	cases := []struct{ id, want string }{
		{"gpt-6-1-sol", "databricks-gpt-6-1-sol"},
		{"glm-5-3", "databricks-glm-5-3"},                         // a version-looking id is never rewritten
		{"databricks-glm-5-3", "databricks-glm-5-3"},              // already wire: no double prefix
		{"new-live-only-model", "databricks-new-live-only-model"}, // outside the catalog still takes the row's prefix
	}
	for _, c := range cases {
		if got := menuWire(p, c.id); got != c.want {
			t.Errorf("menuWire(%q) = %q, want %q", c.id, got, c.want)
		}
	}
}

func TestCodexSlug(t *testing.T) {
	for id, want := range map[string]string{"gpt-6-1-sol": "gpt-6.1-sol", "gpt-6-luna": "gpt-6-luna", "glm-5-3": "glm-5.3", "unknown-9-9": "unknown-9-9"} {
		if got := codexSlug(id); got != want {
			t.Errorf("codexSlug(%q) = %q, want %q", id, got, want)
		}
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

// runNoTools runs a NoTools claude one-shot against a fake Databricks server and returns the paths it served.
func runNoTools(t *testing.T, model string, resp http.HandlerFunc, out *strings.Builder) (Result, []string, error) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		resp(w, r)
	}))
	defer srv.Close()
	dbxServing(t, srv)
	res, err := claudeRunner{}.Run(context.Background(), RunOptions{Prompt: "hi", Provider: "dbx", Model: model, NoTools: true}, NewTextSink(out))
	return res, paths, err
}

// NoTools one-shot routing on a Databricks row: Claude rides /anthropic, other chat models /v1, Responses-only is refused.
func TestClaudeNoToolsRouting(t *testing.T) {
	chat := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "wrong route", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"model\":\"kimi-k3\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}
	claude := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}
	cases := []struct {
		name, model, wantOut string
		resp                 http.HandlerFunc
		wantPaths            []string
	}{
		{"non-claude model uses the openai chat route", "databricks-kimi-k3", "ok", chat, []string{"/v1/chat/completions"}},
		{"claude model uses the anthropic route", "databricks-claude-haiku-4-5", "", claude, []string{"/anthropic/v1/messages"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out strings.Builder
			res, paths, err := runNoTools(t, c.model, c.resp, &out)
			if err != nil || res.Provider != "dbx" || (c.wantOut != "" && out.String() != c.wantOut) {
				t.Fatalf("res = %+v err = %v out %q", res, err, out.String())
			}
			if !slices.Equal(paths, c.wantPaths) {
				t.Errorf("paths = %v, want %v", paths, c.wantPaths)
			}
		})
	}
}

// A Responses-only model is refused before any request leaves.
func TestClaudeNoToolsRefusesResponsesOnly(t *testing.T) {
	var out strings.Builder
	_, paths, err := runNoTools(t, "databricks-gpt-5-5", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "databricks-gpt-5-5") {
		t.Errorf("err = %v, want it to name the model", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want none", paths)
	}
}
