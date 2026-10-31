package agent

import (
	"context"
	"fmt"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

type geminiEngine struct{ baseEngine }

var _ headlessEngine = geminiEngine{}

func (geminiEngine) attach(prompt string, paths []string, tools bool) (string, error) {
	if tools {
		return attachSuffix(prompt, paths), nil
	}
	return attachInline(prompt, paths)
}

func (geminiEngine) before(req RunOptions, t providers.Target) providers.Target {
	t.Model = agyModel(req, t)
	return t
}

func (geminiEngine) exec(ctx context.Context, p vendorPrep, req RunOptions, sink Sink) (Result, error) {
	areq := p.agyRequest(req)
	res := p.res
	res.Model = areq.Model
	out, err := runAgy(ctx, areq, sink)
	res.Answer, res.Usage, res.Transport, res.Structured = out.Answer, out.Usage, out.Transport, out.Structured
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	return res, err
}

func (geminiEngine) login(_ *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	return geminiLogin(model, h)
}

// wire serves only a gateway row: providers.Serves refuses agy every other non-login row.
func (geminiEngine) wire(ctx context.Context, r wireRoute, h *Headless) ([]string, []string, error) {
	if isDirect(ctx) { // --direct skips the router that swaps the Databricks token
		return nil, nil, fmt.Errorf("gemini: --direct has no Databricks route for agy (profile %s needs the router's token swap)", r.Provider.Name)
	}
	return geminiGateway(ctx, r.Provider, r.Target.Model, h) // always routed: the router fixes agy's requests for Databricks
}

func (e geminiEngine) fallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	if p, ok := loginProvider(reg, "google", e.name); ok && GeminiLoginPresent() {
		// the GEMINI_MODEL pin beats the registry's bare slot id
		t, _ := providers.ResolveAgentModel(providers.ModelAsk{Agent: e.name, Target: providers.Target{Provider: p, Model: p.Models[models.Opus]}})
		return t, "gemini (Google login via agy)", true
	}
	return providers.Target{}, "", false
}

func (geminiEngine) LoginStatus() (bool, string) {
	return GeminiLoginPresent(), "Google login (agy)"
}

func (geminiEngine) newLive() liveBackend { return &agyLive{} }
