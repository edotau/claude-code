package vendors

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// ResolveTarget picks the target a vendor launch uses: sel's target when it can serve the vendor
// (a login provider naming it, or a route on one of its dialects), else — only when the caller named
// no provider itself (explicit is false) — the vendor's own subscription login or first keyed
// provider with a resolvable credential. note is a one-line explanation to print on fallback.
func ResolveTarget(reg *providers.Registry, name string, sel providers.Selection, explicit bool) (target providers.Target, note string, err error) {
	t, ok := sel.Slots[models.Opus]
	if !ok {
		return providers.Target{}, "", fmt.Errorf("%s: no opus target resolved", name)
	}
	// "OpenCode defaults to the Copilot plan": opencode's own github-copilot login (unrelated to the
	// session provider) is preferred whenever it is present, ahead of a session provider that could
	// otherwise already serve opencode; an explicit --provider still wins.
	if name == "opencode" && !explicit {
		if OpencodeOwnLoginPresent() { // opencode's own Go/Zen credential: launch plain on its own config
			return providers.Target{}, "", ErrOwnLogin
		}
		if gt, gnote, ok := opencodeCopilotTarget(reg); ok {
			return gt, gnote, nil
		}
	}
	if explicit || (providers.CanServe(name, t.Provider) && geminiFollowsPin(name, t.Provider)) {
		if name == "copilot" {
			return CopilotGate(reg, t)
		}
		return t, "", nil
	}
	if ft, fallbackNote, ok := fallbackTarget(reg, name); ok {
		note = fmt.Sprintf("%s: session provider %s cannot serve %s; using %s", name, t.Provider.Name, name, fallbackNote)
		if name == "copilot" && ft.Provider != nil && ft.Provider.Preset == providers.PresetDatabricks {
			note = fallbackNote // the gate's own notice already names the account and the profile
		}
		if ft.Provider == nil {
			return ft, note, ErrOwnLogin // the vendor's own stored login: launch it plain, nothing to inject
		}
		return ft, note, nil
	}
	// opencode and copilot have a defined fallback chain (DescribeRoute); nothing in it resolved either.
	if name == "opencode" || name == "copilot" {
		if ri := DescribeRoute(reg, name); !ri.OK {
			return providers.Target{}, "", errors.New(ri.Reason)
		}
	}
	if name == "gemini" && providers.GeminiGatewayRow(t.Provider) { // Databricks only on a GEMINI_PROFILE pin, never silently
		return providers.Target{}, "", errors.New("gemini: agy needs a Google login (agy auth login) or a GEMINI_PROFILE pin")
	}
	return t, "", nil
}

// CopilotGate keeps t unless it is copilot's GitHub login on an unworkable pin; then the Databricks fallback and its
// notice — never another GitHub account.
func CopilotGate(reg *providers.Registry, t providers.Target) (providers.Target, string, error) {
	if t.Provider == nil || t.Provider.Auth.Login != "copilot" {
		return t, "", nil
	}
	detail, ok := copilotHeadroom(context.Background(), CopilotPinLogin())
	if ok {
		return t, "", nil
	}
	return CopilotFallback(reg, detail)
}

// CopilotFallback is the copilot Databricks fallback target and its notice; detail names why GitHub was refused.
func CopilotFallback(reg *providers.Registry, detail string) (providers.Target, string, error) {
	ri := copilotDatabricksFallback(reg, detail)
	ft, ok := routeTarget(reg, ri)
	if !ok {
		return providers.Target{}, "", errors.New(ri.Reason)
	}
	return ft, ri.Reason, nil
}

// copilotDatabricksFallback: this session's (else the vault-pinned) profile when it is a signed-in Databricks row,
// else the first Databricks profile whose credential resolves, else LegNone. detail names why GitHub was refused.
func copilotDatabricksFallback(reg *providers.Registry, detail string) RouteInfo {
	pick := func(p *providers.Provider) RouteInfo {
		return RouteInfo{Agent: "copilot", Leg: LegDatabricks, Provider: p.Name, Model: p.Models[models.Opus],
			Reason: "copilot: " + detail + " — using databricks profile " + p.Name, OK: true}
	}
	pin := cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile))
	for _, name := range append([]string{pin}, providers.DatabricksProfiles(reg)...) {
		if p := reg.Providers[name]; databricksUsable(p) {
			return pick(p)
		}
	}
	return RouteInfo{Agent: "copilot", Leg: LegNone,
		Reason: "copilot: " + detail + "; no credit-free route for copilot (no Databricks profile signed in)"}
}

// databricksUsable is true for a Databricks row whose credential resolves.
func databricksUsable(p *providers.Provider) bool {
	if p == nil || p.Preset != providers.PresetDatabricks {
		return false
	}
	_, err := providers.Credential(context.Background(), p)
	return providers.Verdict(err) == providers.VerdictOK
}

// ErrOwnLogin means the vendor should run on the credentials its own CLI stores (no harness route injected).
var ErrOwnLogin = errors.New("agent runs on its own login")

// fallbackTarget is each vendor's own route to a working credential when the session provider has none.
func fallbackTarget(reg *providers.Registry, name string) (providers.Target, string, bool) {
	switch name {
	case "codex":
		if p, ok := loginProvider(reg, "openai", "codex"); ok && codexChatGPTLogin() == nil {
			// codex's own config.toml model beats the catalog: the ChatGPT account only serves what codex itself lists.
			model := p.Models[models.Opus]
			if own := codexOwnModel(); own != "" {
				model = own
			}
			t, _ := providers.ResolveAgentModel(providers.ModelAsk{Agent: name, Target: providers.Target{Provider: p, Model: model}}) // pin beats both
			return t, "chatgpt (codex login)", true
		}
	case "copilot":
		// copilot's own chain (DescribeRoute): its GitHub login, then BYOK with an anthropic or gemini key.
		if ri := DescribeRoute(reg, "copilot"); ri.OK {
			if t, ok := routeTarget(reg, ri); ok {
				return t, ri.Reason, true
			}
		}
	case "gemini":
		if p, ok := loginProvider(reg, "google", "gemini"); ok && GeminiLoginPresent() {
			// the GEMINI_MODEL pin beats the registry's bare slot id
			t, _ := providers.ResolveAgentModel(providers.ModelAsk{Agent: name, Target: providers.Target{Provider: p, Model: p.Models[models.Opus]}})
			return t, "gemini (Google login via agy)", true
		}
	case "opencode":
		// opencode's own chain (DescribeRoute): its own GitHub Copilot login, then an anthropic or gemini key.
		if ri := DescribeRoute(reg, "opencode"); ri.OK {
			if t, ok := routeTarget(reg, ri); ok {
				return t, ri.Reason, true
			}
		}
	}
	return providers.Target{}, "", false
}

// keyProvider is the first profile under provider that bills on a harness-held key.
func keyProvider(reg *providers.Registry, provider string) (*providers.Provider, bool) {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.APIKeyAuth() {
			return p, true
		}
	}
	return nil, false
}

// loginProvider is the first profile under provider whose login serves vendor.
func loginProvider(reg *providers.Registry, provider, vendor string) (*providers.Provider, bool) {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.Auth.Login == vendor {
			return p, true
		}
	}
	return nil, false
}
