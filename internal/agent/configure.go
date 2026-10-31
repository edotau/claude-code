package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

// Credentials ride these env vars (never argv); the config text names the variable, not the secret.
const (
	CodexKeyEnv    = "HARNESS_CODEX_API_KEY"
	OpenCodeKeyEnv = "HARNESS_OPENCODE_API_KEY"
)

// Configure returns extra env (KEY=VALUE) and leading argv that point the agent CLI at target.
// headlessPrompt != "" asks for the non-interactive form (the runner path).
func (s Schema) Configure(ctx context.Context, target providers.Target, headlessPrompt string) (env, argv []string, err error) {
	if headlessPrompt == "" {
		return s.configure(ctx, target, nil)
	}
	return s.configure(ctx, target, &Headless{Prompt: headlessPrompt})
}

// configureHeadless is Configure's runner form with the per-run options each CLI can carry.
func (s Schema) configureHeadless(ctx context.Context, target providers.Target, h Headless) (env, argv []string, err error) {
	if err := checkPrompt(h); err != nil {
		return nil, nil, err
	}
	return s.configure(ctx, target, &h)
}

// configure resolves the route for the agent, then applies its spec launch env/args — on every
// route (own login, BYOK, gateway, router) — so no path can skip it; h != nil (a headless leg) never
// gets Launch.Args, tools-off must stay tools-off and a tools-on leg already carries its own flags.
func (s Schema) configure(ctx context.Context, t providers.Target, h *Headless) ([]string, []string, error) {
	env, argv, err := s.configureRoute(ctx, t, h)
	if err != nil {
		return env, argv, err
	}
	// Route keys go last and shadow same-named launch.env entries, so a spec overlay can never redirect an agent.
	launchEnv, argv := s.applyLaunch(nil, argv, h == nil)
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

// applyLaunch appends the agent's spec Launch.Env (always) and Launch.Args (only when args is true) to
// an already-built launch; a broken overlay is best-effort and must not break the launch.
func (s Schema) applyLaunch(env, argv []string, args bool) ([]string, []string) {
	env = append(env, s.eng().launchEnv()...)
	vc, err := LoadConfig()
	if err != nil {
		return env, argv
	}
	launch := vc.Agent(s.Name).Launch
	env = append(env, launch.Env...)
	if args {
		argv = append(argv, launch.Args...)
	}
	return env, argv
}

// LaunchSpec is the agent's spec env and argv prefix for an interactive launch that bypasses Configure (own login).
func (s Schema) LaunchSpec() (env, argv []string) { return s.applyLaunch(nil, nil, true) }

func (s Schema) configureRoute(ctx context.Context, t providers.Target, h *Headless) ([]string, []string, error) {
	name := s.Name
	e, ok := engines[name]
	if !ok {
		return nil, nil, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(Names(), "|"))
	}
	p := t.Provider
	if p == nil || t.Model == "" {
		return nil, nil, fmt.Errorf("%s: no provider/model resolved", name)
	}
	if p.Auth.Login != "" {
		return login(e, p, models.Strip1M(t.Model), h)
	}
	if len(s.CLI.Dialects) == 0 && GeminiGatewayRow(p) { // a CLI with no dialect reaches a gateway row only here
		return e.wire(ctx, wireRoute{Target: t, Provider: p}, h)
	}
	if ok, why := Serves(name, p, ""); !ok {
		return nil, nil, fmt.Errorf("%s: %s", name, why)
	}
	dialect, base := "", ""
	for _, d := range s.CLI.Dialects {
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
		// Keyed provider: the agent gets the router base + router secret; the real credential stays in the router.
		if p, base, cred, err = routerRoute(ctx, s, p, dialect); err != nil {
			return nil, nil, err
		}
		routed = true
	} else if cred, err = providers.Credential(ctx, p); err != nil {
		return nil, nil, err
	}
	return e.wire(ctx, wireRoute{Target: t, Provider: p, Dialect: dialect, Base: base, Model: model, Cred: cred, Routed: routed}, h)
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

// WithDirect makes configure skip the router and hand the agent the provider's own route and credential.
func WithDirect(ctx context.Context) context.Context {
	return context.WithValue(ctx, directKey{}, true)
}

func isDirect(ctx context.Context) bool { v, _ := ctx.Value(directKey{}).(bool); return v }

// routerRoute ensures the router daemon and returns a copy of p routed through it with the router secret as key.
func routerRoute(ctx context.Context, s Schema, p *providers.Provider, dialect string) (*providers.Provider, string, string, error) {
	rb, err := ensureRouter(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: router: %w", s.Name, err)
	}
	secret, err := routerSecret()
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: router secret: %w", s.Name, err)
	}
	base := proxy.SessionBase(rb, p.Name) + s.CLI.RouterSuffix[dialect]
	rp := *p
	rp.Headers = map[string]string{proxy.AgentHeader: s.Name} // the router adds the provider's own headers upstream
	return &rp, base, secret, nil
}

// positional guards a dash-led prompt from flag parsing.
func positional(prompt string) []string {
	if strings.HasPrefix(prompt, "-") {
		return []string{"--", prompt}
	}
	return []string{prompt}
}
