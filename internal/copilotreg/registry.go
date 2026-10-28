package copilotreg

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// Provider is one named upstream: Type anthropic|openai, WireAPI "" (anthropic), completions or responses.
type Provider struct {
	Name    string
	Type    string
	WireAPI string
	BaseURL string
	Bearer  bool // credential goes in bearerToken, else apiKey
	Headers map[string]string
}

// Model is one selectable entry; the CLI selects it as Provider + "/" + ID.
type Model struct {
	ID                     string // canonical
	Provider               string
	WireModel              string // the Databricks endpoint id
	ModelID                string // copilot's catalog form, so its per-model settings apply
	Name                   string
	Tested                 bool
	MaxPromptTokens        int
	MaxContextWindowTokens int
	MaxOutputTokens        int
}

// Registry is the credential-free registry data; the SDK path maps it in memory, the CLI path renders it.
type Registry struct {
	Profile   string
	Providers []Provider
	Models    []Model
}

// Ref is the CLI's selection string for m.
func (m Model) Ref() string { return m.Provider + "/" + m.ID }

// wireRoute is the router route and registry shape of one wire.
var wireRoute = map[string]struct{ suffix, typ, wireAPI string }{
	providers.ProtocolAnthropic: {"", "anthropic", ""},
	providers.ProtocolOpenAI:    {"/openai", "openai", "completions"},
	providers.ProtocolResponses: {"/responses", "openai", "responses"},
}

// Build maps profile p's endpoints to registry data against routerBase (the router root, no /p/<profile>).
// An endpoint whose canonical id repeats an earlier one is dropped, so every id selects one model.
func Build(profile string, p *providers.Provider, eps []Endpoint, routerBase string) Registry {
	r := Registry{Profile: profile}
	base := strings.TrimRight(routerBase, "/") + "/p/" + profile
	seen, used := map[string]bool{}, map[string]bool{}
	for _, ep := range eps {
		wire, canonical := NonClaudeWire(p, providers.ProtocolAnthropic, ep.ID, true)
		if wire == "" {
			wire, canonical = providers.ProtocolAnthropic, canonicalOf(ep.ID)
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		name := profile + "-" + wire
		if !used[wire] {
			used[wire] = true
			r.Providers = append(r.Providers, newProvider(name, wire, base, p))
		}
		window, output := Caps(providers.ContextWindow(p, ep.ID), ep.ID)
		r.Models = append(r.Models, Model{
			ID: canonical, Provider: name, WireModel: ep.ID, ModelID: WellKnownModel(canonical), Name: ep.Name,
			Tested: ep.Tested, MaxPromptTokens: PromptTokens(window, output), MaxContextWindowTokens: window,
			MaxOutputTokens: output,
		})
	}
	return r
}

func newProvider(name, wire, base string, p *providers.Provider) Provider {
	rt := wireRoute[wire]
	return Provider{Name: name, Type: rt.typ, WireAPI: rt.wireAPI, BaseURL: base + rt.suffix,
		Bearer: wire != providers.ProtocolAnthropic, Headers: p.Headers}
}

func canonicalOf(endpoint string) string {
	if c, _, ok := models.Resolve(endpoint); ok {
		return c
	}
	return strings.TrimPrefix(endpoint, models.DatabricksPrefix)
}

type jsonProvider struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	WireAPI     string            `json:"wireApi,omitempty"`
	BaseURL     string            `json:"baseUrl"`
	APIKey      string            `json:"apiKey,omitempty"`
	BearerToken string            `json:"bearerToken,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

type jsonModel struct {
	ID                     string `json:"id"`
	Provider               string `json:"provider"`
	WireModel              string `json:"wireModel"`
	ModelID                string `json:"modelId"`
	Name                   string `json:"name"`
	MaxPromptTokens        int    `json:"maxPromptTokens,omitempty"`
	MaxContextWindowTokens int    `json:"maxContextWindowTokens,omitempty"`
	MaxOutputTokens        int    `json:"maxOutputTokens,omitempty"`
}

// Render is the CLI's COPILOT_PROVIDERS_CONFIG JSON with cred (the session token) on every provider.
func Render(r Registry, cred string) ([]byte, error) {
	if len(r.Models) == 0 {
		return nil, errors.New("copilotreg: no models to render")
	}
	out := struct {
		Providers []jsonProvider `json:"providers"`
		Models    []jsonModel    `json:"models"`
	}{Providers: []jsonProvider{}, Models: []jsonModel{}}
	for _, p := range r.Providers {
		jp := jsonProvider{Name: p.Name, Type: p.Type, WireAPI: p.WireAPI, BaseURL: p.BaseURL, Headers: p.Headers}
		if p.Bearer {
			jp.BearerToken = cred
		} else {
			jp.APIKey = cred
		}
		out.Providers = append(out.Providers, jp)
	}
	for _, m := range r.Models {
		out.Models = append(out.Models, jsonModel{ID: m.ID, Provider: m.Provider, WireModel: m.WireModel,
			ModelID: m.ModelID, Name: m.Name, MaxPromptTokens: m.MaxPromptTokens,
			MaxContextWindowTokens: m.MaxContextWindowTokens, MaxOutputTokens: m.MaxOutputTokens})
	}
	return json.MarshalIndent(out, "", "  ")
}
