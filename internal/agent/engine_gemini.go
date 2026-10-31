package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

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

// followsPin keeps agy on its Google login unless GEMINI_PROFILE names the session's Databricks row.
func (geminiEngine) followsPin(p *providers.Provider) bool {
	return !providers.GeminiGatewayRow(p) || providers.Pin(providers.PinGeminiProfile) == p.Name
}

// noRoute refuses a Databricks gateway row that no GEMINI_PROFILE pin selected: never silently.
func (geminiEngine) noRoute(_ *providers.Registry, t providers.Target) error {
	if providers.GeminiGatewayRow(t.Provider) {
		return errors.New("gemini: agy needs a Google login (agy auth login) or a GEMINI_PROFILE pin")
	}
	return nil
}

func (geminiEngine) checkLogin(context.Context) (providers.AuthVerdict, string) {
	if geminiPresent() {
		return providers.VerdictOK, "agy login present"
	}
	return providers.VerdictNeedsLogin, ""
}

func (geminiEngine) render(_ *providers.Registry, sel providers.Selection, home string, o RenderOptions, out io.Writer) (bool, error) {
	d, err := reportGemini(sel, out)
	if err != nil {
		return d, err
	}
	gd, err := renderGeminiSettings(home, o, out)
	return d || gd, err
}

func (geminiEngine) configPaths(home string) ([]labelPath, error) {
	home, err := homeOr(home)
	if err != nil {
		return nil, err
	}
	return []labelPath{{"gemini-settings", filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")}}, nil
}

func (geminiEngine) routeDetail() (string, bool) { return geminiRouteDetail(), true }

func (geminiEngine) newLive() liveBackend { return &agyLive{} }
