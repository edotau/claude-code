package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

type copilotEngine struct{ baseEngine }

var _ engine = copilotEngine{}

func (copilotEngine) login(_ *providers.Provider, model string, h *Headless) (env, argv []string, err error) {
	return copilotLogin(model, h)
}

func (copilotEngine) wire(_ context.Context, r wireRoute, h *Headless) (env, argv []string, err error) {
	client := providers.Target{Provider: r.Target.Provider, Model: r.Target.Model, OneM: r.Target.OneM}.ClientModel()
	return copilotWire(r.Provider, r.Dialect, r.Base, r.Model, providers.ContextWindow(r.Target.Provider, client), r.Cred, r.Routed, h)
}

func (e copilotEngine) fallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	return routeFallback(reg, e.describeRoute(reg))
}

func (copilotEngine) describeRoute(reg *providers.Registry) RouteInfo {
	return describeCopilotRoute(reg)
}

func (copilotEngine) LoginStatus() (bool, string) {
	if a, ok := ResolveCopilotAuth(); ok {
		return true, a.Label()
	}
	return false, ""
}

func (copilotEngine) gate(reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	return copilotGate(reg, t)
}

// gateExplicit also turns a gate move off the pinned GitHub profile into a quota error and keeps the requested tier.
func (e copilotEngine) gateExplicit(req RunOptions, reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	from := t.Provider
	t, note, err := copilotGate(reg, t)
	if err == nil && t.Provider != from && req.Provider != "" && req.Provider == providers.Pin(providers.PinCopilotProfile) {
		return t, note, &Error{Kind: KindQuota, Agent: e.name, Err: errors.New(note)}
	}
	if tier := resumeSlot(req.Model); err == nil && t.Provider != from && t.Provider.Models[tier] != "" {
		t.Model = t.Provider.Models[tier] // the fallback serves the requested tier, not its default opus
	}
	return t, note, err
}

func (e copilotEngine) noRoute(reg *providers.Registry, _ providers.Target) error {
	return routeErr(e.describeRoute(reg))
}

// fallbackNote keeps the Databricks fallback's own notice: the gate already names the account and the profile.
func (copilotEngine) fallbackNote(ft providers.Target, note string) (string, bool) {
	return note, ft.Provider != nil && ft.Provider.Preset == providers.PresetDatabricks
}

func (copilotEngine) presence(router.Cell) error {
	if u := copilotPinLogin(); u != "" && !slices.Contains(GHAccounts(), u) {
		return fmt.Errorf("pinned gh user %s is not logged in (gh auth login); no other account is used", u)
	}
	return nil
}

// loginPresent stays local to copilot: LoginStatus's label may call api.github.com.
func (copilotEngine) loginPresent() bool { return CopilotLoginPresent() }

func (copilotEngine) checkLogin(context.Context) (providers.AuthVerdict, string) {
	if l := signinCopilotLogin(); l != "" {
		return providers.VerdictOK, "signed in as " + l
	}
	return providers.VerdictNeedsLogin, ""
}

func (copilotEngine) render(reg *providers.Registry, sel providers.Selection, home string, o RenderOptions, out io.Writer) (bool, error) {
	return renderCopilot(reg, sel, home, o, out)
}

func (copilotEngine) configPaths(home string) ([]labelPath, error) {
	home, err := homeOr(home)
	if err != nil {
		return nil, err
	}
	return []labelPath{
		{"copilot-hooks", filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")},
		{"copilot-settings", filepath.Join(home, ".copilot", "settings.json")},
	}, nil
}

func (copilotEngine) nativeRegistry(ctx context.Context, t providers.Target) bool {
	return UsesCopilotRegistry(ctx, t)
}

func (copilotEngine) routeDetail() (string, bool) {
	reg, _, selErr, ok := loadSelection()
	if !ok {
		return "", false
	}
	return copilotRouteDetail(reg, selErr), true
}
