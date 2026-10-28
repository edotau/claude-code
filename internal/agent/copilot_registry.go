package agent

import (
	"context"
	"fmt"
	"os"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/edotau/claude-code/internal/copilotreg"
	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vendors"
)

// Seams for the registry route: tests stub the router, the session mint and endpoint discovery.
var (
	copilotEnsureRouter = proxy.Ensure
	copilotMintToken    = proxy.MintSessionToken
	copilotRevokeToken  = proxy.RevokeSessionToken
	copilotDiscover     = copilotreg.Discover
)

// copilotRegistryRoute is the multi-provider SDK wiring for one run: in-memory providers, a session token, no file.
type copilotRegistryRoute struct {
	providers []copilot.NamedProviderConfig
	models    []copilot.ProviderModelConfig
	ref       string // the selected provider/id
	token     string
	home      vendors.CopilotPrivateHome
}

// copilotRegistryEligible: a keyed Databricks profile routed through the proxy (not --direct, not a GitHub login).
func copilotRegistryEligible(req Request, t providers.Target) bool {
	p := t.Provider
	return p != nil && !req.Direct && p.Preset == providers.PresetDatabricks && p.Auth.Login == "" &&
		p.Auth.Method() != providers.AuthNone && p.Auth.Method() != providers.AuthCLI
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
	cr := copilotreg.Build(t.Provider.Name, &rp, eps, base)
	ref, err := cr.Select(copilotreg.Choice{Requested: []string{models.Strip1M(t.Model)}})
	if err != nil {
		return nil, fmt.Errorf("copilot: %w", err)
	}
	home, err := vendors.PrepareCopilotHome(false)
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
	o.ChildEnv, o.ChildUnset, o.ChildUnsetPrefix = rt.home.Env, rt.home.Unset, vendors.CopilotStripPrefix
}

// close revokes the run's token; the daemon also expires it, so a failure is not worth failing the run.
func (rt *copilotRegistryRoute) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = copilotRevokeToken(ctx, rt.token)
}
