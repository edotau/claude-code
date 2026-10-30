package copilotreg

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// gw is an ai-gateway row: anthropic and responses routes, no openai route.
func gw() *providers.Provider {
	h := "https://h.example.com/ai-gateway"
	return &providers.Provider{Preset: providers.PresetDatabricks, Routes: map[string]string{
		providers.ProtocolAnthropic: h + "/anthropic", providers.ProtocolResponses: h + "/codex/v1"}}
}

// D5: an endpoint the row has no route for is left out of the registry and named in Note.
func TestBuildGatewayDropsChatEndpoints(t *testing.T) {
	eps := []Endpoint{ep("databricks-claude-sonnet-5-5"), ep("databricks-gpt-5-5"),
		ep("databricks-meta-llama-3-3-70b-instruct"), ep("databricks-kimi-k3")}
	r := Build("gwdev", gw(), eps, "http://r")
	out := render(t, r, "cct_x")
	for _, p := range out.Providers {
		if p["name"] == "gwdev-openai" {
			t.Errorf("openai chat provider on a row without an openai route: %v", p)
		}
	}
	ids := map[string]bool{}
	for _, m := range out.Models {
		ids[m["id"].(string)] = true
	}
	if len(out.Models) != 2 || !ids["claude-sonnet-5-5"] || !ids["gpt-5-5"] {
		t.Errorf("models = %v, want only claude and gpt", out.Models)
	}
	for _, want := range []string{"gwdev", "2 endpoint(s)", "databricks-meta-llama-3-3-70b-instruct", "databricks-kimi-k3"} {
		if !strings.Contains(r.Note, want) {
			t.Errorf("Note = %q, missing %q", r.Note, want)
		}
	}
}

// A serving row serves every endpoint and sets no Note.
func TestBuildServingKeepsChatEndpoints(t *testing.T) {
	r := Build("dev", dbx(), []Endpoint{ep("databricks-claude-sonnet-5-5"), ep("databricks-kimi-k3")}, "http://r")
	if len(r.Models) != 2 || r.Note != "" {
		t.Errorf("models = %v, note %q", r.Models, r.Note)
	}
	if w, _, ok, why := EndpointWire(dbx(), "databricks-kimi-k3"); !ok || w != providers.ProtocolOpenAI || why != "" {
		t.Errorf("EndpointWire(kimi) = %q,%v,%q", w, ok, why)
	}
	if w, _, ok, _ := EndpointWire(gw(), "databricks-kimi-k3"); ok || w != "" {
		t.Errorf("EndpointWire(gateway kimi) = %q,%v, want unservable", w, ok)
	}
}
