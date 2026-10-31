package agent

import (
	"context"
	"errors"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

type opencodeEngine struct{ baseEngine }

var _ headlessEngine = opencodeEngine{}

func (opencodeEngine) after(p *vendorPrep, req RunOptions) {
	if req.SystemPrompt != "" {
		p.env, p.argv = opencodeSystemPrompt(p.env, p.argv, req.SystemPrompt)
	}
}

func (opencodeEngine) exec(ctx context.Context, p vendorPrep, req RunOptions, sink Sink) (Result, error) {
	s, _ := LookupSchema("opencode")
	bin, err := paths.VendorBin(s.CLI.Binary)
	if err != nil {
		return p.res, &Error{Kind: KindFatal, Agent: "opencode", Err: err}
	}
	cmd, tail := childCmd(ctx, bin, p.argv, p.env, req.WorkDir, "", req.KeepEnv, true)
	return runStream(ctx, "opencode", cmd, tail, p.res, sink, parseOpenCodeStream)
}

// loginServes adds github: that profile's Auth.Login names copilot, yet opencode has its own GitHub Copilot login.
func (e opencodeEngine) loginServes(p *providers.Provider) bool {
	return p.Provider == "github" || e.baseEngine.loginServes(p)
}

func (opencodeEngine) login(p *providers.Provider, model string, h *Headless) (env, argv []string, err error) {
	if p.Provider != "github" {
		return nil, nil, errors.New("opencode: no subscription sign-in support")
	}
	if model == "auto" && p.OpencodeFallback != "" { // Copilot's own router id; opencode's catalog lacks it
		model = p.OpencodeFallback
	}
	return opencodeCopilotLogin(model, h)
}

func (opencodeEngine) wire(ctx context.Context, r wireRoute, h *Headless) (env, argv []string, err error) {
	return opencode(r.Provider, r.Dialect, r.Base, r.Model, r.Cred, r.Routed, h)
}

func (e opencodeEngine) fallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	return routeFallback(reg, e.describeRoute(reg))
}

func (opencodeEngine) describeRoute(reg *providers.Registry) RouteInfo {
	return describeOpenCodeRoute(reg)
}

func (opencodeEngine) LoginStatus() (bool, string) {
	if names := OpencodeLogins(); len(names) > 0 {
		return true, "opencode auth login: " + strings.Join(names, ", ")
	}
	return false, ""
}
