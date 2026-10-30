package vendors

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/settings"
)

// Credentials ride these env vars (never argv); the config text names the variable, not the secret.
const (
	CodexKeyEnv    = "HARNESS_CODEX_API_KEY"
	OpenCodeKeyEnv = "HARNESS_OPENCODE_API_KEY"
)

// ConfigureHint names the exact command to run when no credential resolved for vendor name at all.
func ConfigureHint(name string) string {
	switch name {
	case "codex":
		return "run `codex login` (ChatGPT) or `claude-code configure` → codex → API key"
	case "copilot":
		return "run `claude-code configure github` (Copilot) or set an Anthropic/Gemini key"
	case "opencode":
		return "run `opencode auth login` (opencode Go) or `claude-code configure github` (Copilot)"
	case "gemini":
		return "sign in with `agy` (Google login)"
	default:
		return "run `claude-code configure agents`"
	}
}

// Configure returns extra env (KEY=VALUE) and leading argv that point the vendor CLI at target.
// headlessPrompt != "" asks for the non-interactive form (the runner path).
func Configure(ctx context.Context, name string, target providers.Target, headlessPrompt string) (env, argv []string, err error) {
	if headlessPrompt == "" {
		return configure(ctx, name, target, nil)
	}
	return configure(ctx, name, target, &Headless{Prompt: headlessPrompt})
}

// ConfigureHeadless is Configure's runner form with the per-run options each CLI can carry.
func ConfigureHeadless(ctx context.Context, name string, target providers.Target, h Headless) (env, argv []string, err error) {
	if strings.TrimSpace(h.Prompt) == "" {
		return nil, nil, errors.New("agents: empty prompt")
	}
	return configure(ctx, name, target, &h)
}

// configure resolves the route for name, then applies the vendor's spec launch env/args — on every
// route (own login, BYOK, gateway, router) — so no path can skip it; h != nil (a headless leg) never
// gets Launch.Args, tools-off must stay tools-off and a tools-on leg already carries its own flags.
func configure(ctx context.Context, name string, t providers.Target, h *Headless) ([]string, []string, error) {
	env, argv, err := configureRoute(ctx, name, t, h)
	if err != nil {
		return env, argv, err
	}
	// Route keys go last and shadow same-named launch.env entries, so a spec overlay can never redirect a vendor.
	launchEnv, argv := applyLaunch(name, nil, argv, h == nil)
	return routeEnvLast(launchEnv, env), argv, nil
}

// routeEnvLast is launchEnv minus any key the route env sets, then the route env: the route always wins.
func routeEnvLast(launchEnv, env []string) []string {
	routeKeys := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		routeKeys[k] = true
	}
	var out []string
	for _, kv := range launchEnv {
		if k, _, _ := strings.Cut(kv, "="); !routeKeys[k] {
			out = append(out, kv)
		}
	}
	return append(out, env...)
}

// applyLaunch appends vendor's spec Launch.Env (always) and Launch.Args (only when args is true) to
// an already-built launch; a broken overlay is best-effort and must not break the launch.
func applyLaunch(name string, env, argv []string, args bool) ([]string, []string) {
	if name == "opencode" {
		// Every opencode launch (own login, BYOK, headless) points at the tracked overlay; a failed
		// write is best-effort — opencode still runs on its own global config without it.
		if cfg, tui, err := WriteOpencodeOverlay(); err == nil {
			env = append(env, "OPENCODE_CONFIG="+cfg, "OPENCODE_TUI_CONFIG="+tui)
		}
	}
	vc, err := settings.Load()
	if err != nil {
		return env, argv
	}
	launch := vc.Vendor(name).Launch
	env = append(env, launch.Env...)
	if args {
		argv = append(argv, launch.Args...)
	}
	return env, argv
}

// LaunchSpec is vendor name's spec env and argv prefix for an interactive launch that bypasses Configure (own login).
func LaunchSpec(name string) (env, argv []string) { return applyLaunch(name, nil, nil, true) }

func configureRoute(ctx context.Context, name string, t providers.Target, h *Headless) ([]string, []string, error) {
	spec, ok := Lookup(name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(Names(), "|"))
	}
	p := t.Provider
	if p == nil || t.Model == "" {
		return nil, nil, fmt.Errorf("%s: no provider/model resolved", name)
	}
	if p.Auth.Login != "" {
		return login(name, p, models.Strip1M(t.Model), h)
	}
	if name == "gemini" && GeminiGatewayRow(p) {
		if isDirect(ctx) {
			return nil, nil, fmt.Errorf("gemini: --direct has no Databricks route for agy (profile %s needs the router's token swap)", p.Name)
		}
		return geminiGateway(ctx, p, t.Model, h) // always routed: the router fixes agy's requests for Databricks
	}
	if ok, why := providers.Serves(name, p, ""); !ok {
		return nil, nil, fmt.Errorf("%s: %s", name, why)
	}
	dialect, base := "", ""
	for _, d := range spec.Dialects {
		if r, ok := p.Route(d); ok {
			dialect, base = d, r
			break
		}
	}
	model := p.WireModel(models.Strip1M(t.Model)) // idempotent: targets not built by ResolveAgent get the wire id too
	var cred string
	var err error
	routed := false
	if p.Auth.Method() != providers.AuthNone && !isDirect(ctx) {
		// Keyed provider: the vendor gets the router base + router secret; the real credential stays in the router.
		if p, base, cred, err = routerRoute(ctx, spec, p, dialect); err != nil {
			return nil, nil, err
		}
		routed = true
	} else if cred, err = providers.Credential(ctx, p); err != nil {
		return nil, nil, err
	}
	switch name {
	case "codex":
		return codex(p, base, model, cred, h)
	case "opencode":
		return opencode(p, dialect, base, model, cred, routed, h)
	default:
		// The same resolver as the Claude launch decides [1m]; it reads a prefixed wire id (databricks-claude-…) itself.
		client := providers.Target{Provider: t.Provider, Model: t.Model, OneM: t.OneM}.ClientModel()
		return copilot(p, dialect, base, model, providers.ContextWindow(t.Provider, client), cred, routed, h)
	}
}

// Router seams (tests stub these so no daemon is spawned).
var (
	ensureRouter = proxy.Ensure
	routerSecret = proxy.ClientSecret
)

// StubRouterForTest points configure at base for t: an unstubbed Ensure would relaunch the test binary as the daemon.
func StubRouterForTest(t testing.TB, base string) {
	oe := ensureRouter
	t.Cleanup(func() { ensureRouter = oe })
	ensureRouter = func(context.Context) (string, error) { return base, nil }
}

type directKey struct{}

// WithDirect makes configure skip the router and hand the vendor the provider's own route and credential.
func WithDirect(ctx context.Context) context.Context {
	return context.WithValue(ctx, directKey{}, true)
}

func isDirect(ctx context.Context) bool { v, _ := ctx.Value(directKey{}).(bool); return v }

// routerRoute ensures the router daemon and returns a copy of p routed through it with the router secret as key.
func routerRoute(ctx context.Context, spec Spec, p *providers.Provider, dialect string) (*providers.Provider, string, string, error) {
	rb, err := ensureRouter(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: router: %w", spec.Name, err)
	}
	secret, err := routerSecret()
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: router secret: %w", spec.Name, err)
	}
	base := proxy.SessionBase(rb, p.Name) + spec.RouterSuffix[dialect]
	rp := *p
	rp.Headers = map[string]string{proxy.AgentHeader: spec.Name} // the router adds the provider's own headers upstream
	return &rp, base, secret, nil
}

// positional guards a dash-led prompt from flag parsing.
func positional(prompt string) []string {
	if strings.HasPrefix(prompt, "-") {
		return []string{"--", prompt}
	}
	return []string{prompt}
}
