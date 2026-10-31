package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

// Seams for the registry route: tests stub the router, the session mint and endpoint discovery.
var (
	copilotEnsureRouter = proxy.Ensure
	copilotMintToken    = proxy.MintSessionToken
	copilotRevokeToken  = proxy.RevokeSessionToken
	copilotDiscover     = discoverCopilotEndpoints
)

// copilotRegistryRoute is the multi-provider SDK wiring for one run: in-memory providers, a session token, no file.
type copilotRegistryRoute struct {
	providers []copilot.NamedProviderConfig
	models    []copilot.ProviderModelConfig
	ref       string // the selected provider/id
	token     string
	home      copilotPrivateHome
}

// prepareCopilotRegistry discovers what t's profile serves, selects t.Model and mints a token for this process.
func prepareCopilotRegistry(ctx context.Context, t providers.Target) (*copilotRegistryRoute, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, err
	}
	eps, err := copilotDiscover(ctx, reg, t.Provider)
	if err != nil {
		return nil, fmt.Errorf("copilot: discover %s models: %w", t.Provider.Name, err)
	}
	base, err := copilotEnsureRouter(ctx)
	if err != nil {
		return nil, err
	}
	rp := *t.Provider
	rp.Headers = map[string]string{proxy.AgentHeader: "copilot"} // the router adds the provider's own headers upstream
	cr := copilotBuildRegistry(t.Provider.Name, &rp, eps, base)
	if cr.Note != "" {
		eventLogFor(ctx).emit("note", cr.Note)
	}
	ref, err := cr.Select(copilotChoice([]string{models.Strip1M(t.Model)}))
	if err != nil {
		return nil, fmt.Errorf("copilot: %w", err)
	}
	home, err := PrepareCopilotHome(false)
	if err != nil {
		return nil, err
	}
	tok, err := copilotMintToken(ctx, t.Provider.Name, os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("copilot: mint session token: %w", err)
	}
	rt := &copilotRegistryRoute{ref: ref, token: tok, home: home}
	for _, p := range cr.Providers {
		rt.providers = append(rt.providers, copilot.NamedProviderConfig{Name: p.Name, Type: p.Type, WireAPI: p.WireAPI,
			BaseURL: p.BaseURL, Headers: p.Headers})
	}
	for _, m := range cr.Models {
		rt.models = append(rt.models, copilot.ProviderModelConfig{ID: m.ID, Provider: m.Provider, WireModel: m.WireModel,
			ModelID: m.ModelID, Name: m.Name, MaxPromptTokens: m.MaxPromptTokens,
			MaxContextWindowTokens: m.MaxContextWindowTokens, MaxOutputTokens: m.MaxOutputTokens})
	}
	return rt, nil
}

// apply puts the route on opts; the token reaches the SDK only as a callback result.
func (rt *copilotRegistryRoute) apply(o *copilotsdk.Options) {
	o.Providers, o.Models, o.Model = rt.providers, rt.models, rt.ref
	o.Token = func() (string, error) { return rt.token, nil }
	o.GitHubToken, o.UseLoggedInUser, o.Env = "", false, nil
	o.ChildEnv, o.ChildUnset, o.ChildUnsetPrefix = rt.home.Env, rt.home.Unset, copilotStripPrefix
}

// close revokes the run's token; the daemon also expires it, so a failure is not worth failing the run.
func (rt *copilotRegistryRoute) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = copilotRevokeToken(ctx, rt.token)
}

// CopilotRegistryProvider is one named upstream: Type anthropic|openai, WireAPI "" (anthropic), completions or responses.
type CopilotRegistryProvider struct {
	Name    string
	Type    string
	WireAPI string
	BaseURL string
	Bearer  bool // credential goes in bearerToken, else apiKey
	Headers map[string]string
}

// CopilotRegistryModel is one selectable entry; the CLI selects it as Provider + "/" + ID.
type CopilotRegistryModel struct {
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

// CopilotRegistry is the credential-free registry data; the SDK path maps it in memory, the CLI path renders it.
type CopilotRegistry struct {
	Profile   string
	Providers []CopilotRegistryProvider
	Models    []CopilotRegistryModel
	Note      string // one line naming the endpoints dropped because the profile has no route for them; "" when none
}

// Ref is the CLI's selection string for m.
func (m CopilotRegistryModel) Ref() string { return m.Provider + "/" + m.ID }

// copilotWireRoute is the registry shape of one wire; its router path is copilot's AgentSpec.RouterSuffix.
var copilotWireRoute = map[string]struct{ typ, wireAPI string }{
	providers.ProtocolAnthropic: {"anthropic", ""},
	providers.ProtocolOpenAI:    {"openai", "completions"},
	providers.ProtocolResponses: {"openai", "responses"},
}

// copilotBuildRegistry maps profile p's endpoints to registry data against routerBase (the router root, no /p/<profile>).
// An endpoint whose canonical id repeats an earlier one is dropped, so every id selects one model.
func copilotBuildRegistry(profile string, p *providers.Provider, eps []CopilotEndpoint, routerBase string) CopilotRegistry {
	r := CopilotRegistry{Profile: profile}
	base := strings.TrimRight(routerBase, "/") + "/p/" + profile
	seen, used := map[string]bool{}, map[string]bool{}
	var dropped []string
	for _, ep := range eps {
		wire, canonical, ok, _ := copilotEndpointWire(p, ep.ID)
		if !ok {
			dropped = append(dropped, ep.ID)
			continue
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		name := profile + "-" + wire
		if !used[wire] {
			used[wire] = true
			r.Providers = append(r.Providers, newCopilotRegistryProvider(name, wire, base, p))
		}
		window, output := copilotCaps(providers.ContextWindow(p, ep.ID), ep.ID)
		r.Models = append(r.Models, CopilotRegistryModel{
			ID: canonical, Provider: name, WireModel: ep.ID, ModelID: copilotsdk.WellKnownModel(canonical), Name: ep.Name,
			Tested: ep.Tested, MaxPromptTokens: copilotPromptTokens(window, output), MaxContextWindowTokens: window,
			MaxOutputTokens: output,
		})
	}
	if len(dropped) > 0 {
		r.Note = fmt.Sprintf("copilot: %s has no route for %d endpoint(s), left out of the model menu: %s", profile, len(dropped),
			strings.Join(dropped, ", "))
	}
	return r
}

func newCopilotRegistryProvider(name, wire, base string, p *providers.Provider) CopilotRegistryProvider {
	rt := copilotWireRoute[wire]
	spec, _ := providers.LookupAgentSpec("copilot")
	return CopilotRegistryProvider{Name: name, Type: rt.typ, WireAPI: rt.wireAPI, BaseURL: base + spec.RouterSuffix[wire],
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

// renderCopilotRegistry is the CLI's COPILOT_PROVIDERS_CONFIG JSON with cred (the session token) on every provider.
func renderCopilotRegistry(r CopilotRegistry, cred string) ([]byte, error) {
	if len(r.Models) == 0 {
		return nil, errors.New("copilot registry: no models to render")
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

// Named selection failures; callers match with errors.Is.
var (
	ErrUnknownModel      = errors.New("model is not in the registry")
	ErrAmbiguousModel    = errors.New("model selection is ambiguous")
	ErrSavedModelGone    = errors.New("saved model left the registry")
	ErrNoModels          = errors.New("registry has no models")
	ErrRepeatedSelection = errors.New("model selected more than once")
)

// CopilotChoice is every place a model can come from, highest priority first: Requested (each --model given),
// Saved (a resumed session's provider/id), Pin (the vault pin value); none set picks the default.
type CopilotChoice struct {
	Requested []string
	Saved     string
	Pin       string
}

// Select maps a harness alias | canonical id | wire id | provider/id to the registry's provider/id.
func (r CopilotRegistry) Select(c CopilotChoice) (string, error) {
	if len(r.Models) == 0 {
		return "", ErrNoModels
	}
	switch {
	case len(c.Requested) > 1:
		return "", fmt.Errorf("%w: %s", ErrRepeatedSelection, strings.Join(c.Requested, ", "))
	case len(c.Requested) == 1:
		return r.find(c.Requested[0], ErrUnknownModel)
	case c.Saved != "":
		return r.find(c.Saved, ErrSavedModelGone)
	case c.Pin != "":
		return r.find(c.Pin, ErrSavedModelGone)
	}
	return r.defaultRef(), nil
}

// defaultRef is the sonnet alias's model when the profile serves it, else the first model.
func (r CopilotRegistry) defaultRef() string {
	if canonical, ok := models.Alias(models.Sonnet); ok {
		for _, m := range r.Models {
			if m.ID == canonical {
				return m.Ref()
			}
		}
	}
	return r.Models[0].Ref()
}

// find matches s by provider/id, then id, then wire id, then its canonical resolution; a miss wraps miss.
func (r CopilotRegistry) find(s string, miss error) (string, error) {
	s = strings.TrimSpace(s)
	canonical, _, _ := models.Resolve(s)
	for _, match := range []func(CopilotRegistryModel) bool{
		func(m CopilotRegistryModel) bool { return m.Ref() == s },
		func(m CopilotRegistryModel) bool { return m.ID == s },
		func(m CopilotRegistryModel) bool { return m.WireModel == s },
		func(m CopilotRegistryModel) bool { return canonical != "" && m.ID == canonical },
	} {
		var hits []string
		for _, m := range r.Models {
			if match(m) {
				hits = append(hits, m.Ref())
			}
		}
		switch len(hits) {
		case 0:
		case 1:
			return hits[0], nil
		default:
			return "", fmt.Errorf("%w: %q matches %s", ErrAmbiguousModel, s, strings.Join(hits, ", "))
		}
	}
	return "", fmt.Errorf("%w: %q (profile %s)", miss, s, r.Profile)
}

// CopilotEndpoint is one serving endpoint copilot can reach; Tested marks endpoints on the curated menu.
type CopilotEndpoint struct {
	providers.ModelInfo
	Tested bool
}

// discoverCopilotEndpoints lists the resolved Databricks profile p's READY chat endpoints, marking the curated-menu ones.
func discoverCopilotEndpoints(ctx context.Context, reg *providers.Registry, p *providers.Provider) ([]CopilotEndpoint, error) {
	infos, err := providers.ListModels(ctx, reg, p)
	if err != nil {
		return nil, err
	}
	return copilotServingChatRows(infos, providers.OpencodeMenu()), nil
}

// copilotServingChatRows keeps READY llm/v1/chat endpoints in order, marking those on menu (with or without "databricks-").
func copilotServingChatRows(infos []providers.ModelInfo, menu []providers.OpencodeModel) []CopilotEndpoint {
	onMenu := make(map[string]bool, len(menu))
	for _, m := range menu {
		onMenu[m.ID] = true
	}
	var out []CopilotEndpoint
	for _, info := range infos {
		if info.Task != "llm/v1/chat" || info.Ready != "READY" {
			continue
		}
		tested := onMenu[info.ID] || onMenu[strings.TrimPrefix(info.ID, models.DatabricksPrefix)]
		out = append(out, CopilotEndpoint{ModelInfo: info, Tested: tested})
	}
	return out
}

// copilotEndpointWire is the passthrough wire for model on the Databricks row p (providers.WireFor) and its canonical id; a
// Claude model, or any model on another row, rides anthropic. ok is false, with why, when p has no route serving it.
func copilotEndpointWire(p *providers.Provider, model string) (wire, canonical string, ok bool, why string) {
	canonical = canonicalOf(model)
	if p.Preset != providers.PresetDatabricks || models.FamilyKind(canonical) == models.FamilyClaude {
		return providers.ProtocolAnthropic, canonical, true, ""
	}
	wire, ok, why = providers.WireFor(p, model)
	return wire, canonical, ok, why
}

// copilotMaxOutputTokens is the output cap current Claude/GPT tiers accept; a catalog-less id needs one.
const copilotMaxOutputTokens = 32_000

// copilotPromptTokens is the window minus the output cap and a 10% margin, so copilot compacts before the endpoint
// refuses; 0 (unset) when the window is too small for that.
func copilotPromptTokens(window, output int) int {
	if window <= output {
		return 0
	}
	return window - output - window/10
}

// copilotCaps returns id's context window and output cap: window and copilotMaxOutputTokens, lowered by any documented catalog limit.
func copilotCaps(window int, id string) (ctx, output int) {
	ctx, output = window, copilotMaxOutputTokens
	l, ok := models.Limits(id)
	if !ok {
		return ctx, output
	}
	if l.Context > 0 { // catalog limits only lower the gated window and the output cap, never raise them
		ctx = min(ctx, l.Context)
	}
	if l.Output > 0 {
		output = min(output, l.Output)
	}
	return ctx, output
}
