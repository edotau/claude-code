package agent

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

type opencodeEngine struct{ baseEngine }

var _ headlessEngine = opencodeEngine{}

func (opencodeEngine) after(p *vendorPrep, req RunOptions) {
	if req.SystemPrompt != "" {
		p.env, p.argv = opencodeSystemPrompt(p.env, p.argv, req.SystemPrompt)
	}
}

func (e opencodeEngine) exec(ctx context.Context, p vendorPrep, req RunOptions, sink Sink) (Result, error) {
	bin, err := e.bin()
	if err != nil {
		return p.res, err
	}
	cmd, tail := childCmd(ctx, bin, p.argv, p.env, req.WorkDir, "", req.KeepEnv, true)
	return runStream(ctx, e.name, cmd, tail, p.res, sink, parseOpenCodeStream)
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

// preRoute prefers opencode's own login (Go/Zen, else the github-copilot one) ahead of a session provider that could serve it.
func (opencodeEngine) preRoute(reg *providers.Registry, explicit bool) (providers.Target, string, bool, error) {
	if explicit {
		return providers.Target{}, "", false, nil
	}
	if OpencodeOwnLoginPresent() { // opencode's own Go/Zen credential: launch plain on its own config
		return providers.Target{}, "", true, ErrOwnLogin
	}
	if gt, gnote, ok := opencodeCopilotTarget(reg); ok {
		return gt, gnote, true, nil
	}
	return providers.Target{}, "", false, nil
}

func (e opencodeEngine) noRoute(reg *providers.Registry, _ providers.Target) error {
	return routeErr(e.describeRoute(reg))
}

// launchEnv points every opencode launch at the tracked overlay; a failed write is best-effort, opencode runs without it.
func (opencodeEngine) launchEnv() []string {
	if cfg, tui, err := WriteOpencodeOverlay(); err == nil {
		return []string{"OPENCODE_CONFIG=" + cfg, "OPENCODE_TUI_CONFIG=" + tui}
	}
	return nil
}

func (opencodeEngine) presence(c router.Cell) error {
	if id, _, found := strings.Cut(c.Model, "/"); found {
		_, err := opencodeCredential(id)
		return err
	}
	return nil
}

// ownLogin is opencode on its own stored login/config: argv only, no harness route injected.
// Tools off still rides OPENCODE_CONFIG_CONTENT, which opencode merges over its config.
func (e opencodeEngine) ownLogin(h Headless) (env, argv []string, err error) {
	if err := checkPrompt(h); err != nil {
		return nil, nil, err
	}
	env, argv = opencodeRun("", h)
	if cred, err := opencodeCredential("opencode"); err == nil {
		env = append(env, cred...) // the vault key when opencode stores no login of its own
	}
	launchEnv, argv := schemaFor(e.name).applyLaunch(nil, argv, false) // headless: never Launch.Args
	return routeEnvLast(launchEnv, env), argv, nil
}

func (opencodeEngine) render(_ *providers.Registry, _ providers.Selection, _ string, o RenderOptions, out io.Writer) (bool, error) {
	return renderOpenCodeOverlay(o, out)
}

func (opencodeEngine) routeDetail() (string, bool) {
	reg, sel, selErr, ok := loadSelection()
	if !ok {
		return "", false
	}
	return opencodeRouteDetail(reg, sel, selErr), true
}

// routeErr is ri's reason as an error when ri resolved no route, else nil.
func routeErr(ri RouteInfo) error {
	if !ri.OK {
		return errors.New(ri.Reason)
	}
	return nil
}
