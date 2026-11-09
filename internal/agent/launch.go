package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

// PinnedProfile is agent name's vault-pinned registry profile; "" when unpinned or off, an error when the pin names no row.
func PinnedProfile(name string) (string, error) {
	pins, err := AgentPins()
	if err != nil {
		return "", err
	}
	pin, ok := pins[name]
	if !ok || pin.Off || pin.Profile == "" {
		return "", nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", err
	}
	if err := ValidAgentPin(reg, name, pin.Profile); err != nil {
		return "", fmt.Errorf("pinned profile: %w", err)
	}
	if reg.Providers[pin.Profile] == nil {
		return "", nil // an own-login or PinIDs pin (opencode-go) launches on the agent's own plan
	}
	return pin.Profile, nil
}

// Configure returns extra env (KEY=VALUE) and leading argv that point the agent CLI at target.
// headlessPrompt != "" asks for the non-interactive form (the agent path).
func (s Schema) Configure(ctx context.Context, target providers.Target, headlessPrompt string) (env, argv []string, err error) {
	if headlessPrompt == "" {
		return s.configure(ctx, target, nil)
	}
	return s.configure(ctx, target, &Headless{Prompt: headlessPrompt})
}

// configureHeadless is Configure's agent form with the per-run options each CLI can carry.
func (s Schema) configureHeadless(ctx context.Context, target providers.Target, h Headless) (env, argv []string, err error) {
	if err := CheckPrompt(h); err != nil {
		return nil, nil, err
	}
	return s.configure(ctx, target, &h)
}

// configure resolves the route for the agent, then applies its spec launch env/args — on every
// route (own login, BYOK, gateway, router) — so no path can skip it; h != nil (a headless dispatch) never
// gets Launch.Args, tools-off must stay tools-off and a tools-on dispatch already carries its own flags.
func (s Schema) configure(ctx context.Context, t providers.Target, h *Headless) ([]string, []string, error) {
	env, argv, err := s.configureRoute(ctx, t, h)
	if err != nil {
		return env, argv, err
	}
	// Route keys go last and shadow same-named launch.env entries, so a spec overlay can never redirect an agent.
	launchEnv, argv := s.ApplyLaunch(nil, argv, h == nil)
	return RouteEnvLast(launchEnv, env), argv, nil
}

// RouteEnvLast is launchEnv minus any key the route env sets, then the route env: the route always wins.
func RouteEnvLast(launchEnv, env []string) []string {
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

// ApplyLaunch appends the agent's spec Launch.Env (always) and Launch.Args (only when args is true) to
// an already-built launch; a broken overlay is best-effort and must not break the launch.
func (s Schema) ApplyLaunch(env, argv []string, args bool) ([]string, []string) {
	env = append(env, s.Eng().LaunchEnv()...)
	vc, err := agentconfig.LoadConfig()
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
func (s Schema) LaunchSpec() (env, argv []string) { return s.ApplyLaunch(nil, nil, true) }

func (s Schema) configureRoute(ctx context.Context, t providers.Target, h *Headless) ([]string, []string, error) {
	name := s.Name
	e, ok := driverFor(name)
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
		return e.Wire(ctx, WireRoute{Target: t, Provider: p}, h)
	}
	if ok, why := Serves(name, p, ""); !ok {
		return nil, nil, fmt.Errorf("%s: %s", name, why)
	}
	return s.configureWire(ctx, e, t, h)
}

func (s Schema) configureWire(ctx context.Context, e Driver, t providers.Target, h *Headless) ([]string, []string, error) {
	p := t.Provider
	dialect, base := s.firstRoute(p)
	model := p.WireModel(models.Strip1M(t.Model)) // idempotent: targets not built by ResolveAgent get the wire id too
	var cred string
	var err error
	routed := false
	if p.Auth.Method() != providers.AuthNone && !IsDirect(ctx) {
		// Keyed provider: the agent gets the router base + router secret; the real credential stays in the router.
		if p, base, cred, err = routerRoute(ctx, s, p, dialect); err != nil {
			return nil, nil, err
		}
		routed = true
	} else if cred, err = providers.Credential(ctx, p); err != nil {
		return nil, nil, err
	}
	return e.Wire(ctx, WireRoute{Target: t, Provider: p, Dialect: dialect, Base: base, Model: model, Cred: cred, Routed: routed}, h)
}

func (s Schema) firstRoute(p *providers.Provider) (string, string) {
	for _, d := range s.CLI.Dialects {
		if base, ok := p.Route(d); ok {
			return d, base
		}
	}
	return "", ""
}

// Router seams (tests stub these so no daemon is spawned); EnsureRouter starts or finds the loopback router.
var (
	EnsureRouter = proxy.Ensure
	RouterSecret = proxy.ClientSecret
	MintSession  = proxy.MintSessionToken // mints a router session token for profile and pid
)

type directKey struct{}

// WithDirect makes configure skip the router and hand the agent the provider's own route and credential.
func WithDirect(ctx context.Context) context.Context {
	return context.WithValue(ctx, directKey{}, true)
}

func IsDirect(ctx context.Context) bool { v, _ := ctx.Value(directKey{}).(bool); return v }

// routerRoute ensures the router daemon and returns a copy of p routed through it with the router secret as key.
func routerRoute(ctx context.Context, s Schema, p *providers.Provider, dialect string) (*providers.Provider, string, string, error) {
	rb, err := EnsureRouter(ctx)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: router: %w", s.Name, err)
	}
	secret, err := RouterSecret()
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: router secret: %w", s.Name, err)
	}
	base := proxy.SessionBase(rb, p.Name) + s.CLI.RouterSuffix[dialect]
	rp := *p
	rp.Headers = map[string]string{proxy.AgentHeader: s.Name} // the router adds the provider's own headers upstream
	return &rp, base, secret, nil
}

// login runs the agent on its own subscription sign-in; a login provider serves only the CLI it names.
func login(e Driver, p *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	if !e.LoginServes(p) {
		return nil, nil, fmt.Errorf("%s: provider %s is the %s sign-in; use claude-code run %s --provider %s", e.Name(), p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	}
	return e.Login(p, model, h)
}

// StubRouter points configure at base: an unstubbed Ensure would relaunch the test binary as the daemon.
// Tests call it directly; restore undoes it.
func StubRouter(base string) (restore func()) {
	oe := EnsureRouter
	EnsureRouter = func(context.Context) (string, error) { return base, nil }
	return func() { EnsureRouter = oe }
}

// StubProxyForTest stubs the router, its secret and session mints for a whole test binary (call from TestMain),
// and shortens the dispatch limiter's poll; no proxy daemon is reached or spawned.
func StubProxyForTest(base, secret string) {
	EnsureRouter = func(context.Context) (string, error) { return base, nil }
	RouterSecret = func() (string, error) { return secret, nil }
	MintSession = func(context.Context, string, int) (string, error) { return "", errors.New("session mint is stubbed") }
	retry.SharedPoll = 25 * time.Millisecond
}
