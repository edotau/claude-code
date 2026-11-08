package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

// IsTerminal is the TTY check internal/cli wires at startup; the zero value says no terminal.
var IsTerminal = func(*os.File) bool { return false }

// LaunchPlan resolves `claude-code run <agent>`: the opus target via Configure, else the agent's own login.
func LaunchPlan(ctx context.Context, name string, o LaunchOptions, args []string) (Plan, error) {
	plan, err := cliPlan(ctx, name, o, args)
	plan.Keep, plan.CLI = o.KeepEnv, true
	return plan, err
}

// withFlagModel re-adds the --model ParseArgs consumed, so the registry selects flag > pin > default.
func withFlagModel(args []string, model string) []string {
	if model == "" {
		return args
	}
	return append([]string{"--model", model}, args...)
}

// agentModelTarget applies flag > the agent's model pin > default to t; a fallback provider respells the flag itself.
func agentModelTarget(name string, t providers.Target, sel providers.Selection, flagModel string) (providers.Target, string) {
	applied := t.Provider == nil || t.Provider == sel.Aliases[models.Opus].Provider
	return ResolveAgentModel(ModelAsk{Agent: name, Flag: flagModel, Target: t, Applied: applied})
}

func cliPlan(ctx context.Context, name string, o LaunchOptions, args []string) (Plan, error) {
	if o.Direct {
		ctx = WithDirect(ctx)
	}
	spec, ok := LookupSchema(name)
	if !ok || !spec.External() {
		return Plan{}, fmt.Errorf("unknown agent %q (codex|gemini|opencode|copilot)", name)
	}
	if authArgs(args) {
		return plainPlan(spec, args) // signing in needs no model route; resolving one first blocks the very login it needs
	}
	args, err := spec.eng().LaunchArgs(o.File, args)
	if err != nil {
		return Plan{}, err
	}
	flagModel := o.Model // the flag alone; the agent's model pin is applied after the target resolves
	o, err = launchProfile(name, o)
	if err != nil {
		return Plan{}, err
	}
	reg, sel, err := selection(o)
	if err != nil {
		if hint := spec.eng().ProfileHint(o); hint != nil {
			return Plan{}, hint
		}
		return Plan{}, err
	}
	return cliLaunch(ctx, spec, name, o, args, flagModel, reg, sel)
}

// cliLaunch resolves the target for a selected profile and builds the registry, configured or own-login plan.
func cliLaunch(ctx context.Context, spec Schema, name string, o LaunchOptions, args []string, flagModel string,
	reg *providers.Registry, sel providers.Selection) (Plan, error) {
	// --profile or the agent's own profile pin is explicit: the session pin is Claude Code's default, not a per-agent choice.
	explicit := o.Provider != ""
	target, note, err := cliTarget(spec, name, reg, sel, explicit, flagModel)
	// An own-login note is only worth a line on an interactive launch (or --print-env): `opencode --version` stays quiet.
	showOwnLoginNote := o.PrintEnv || (IsTerminal(os.Stderr) && interactiveArgs(args))
	if errors.Is(err, ErrOwnLogin) {
		return ownLoginPlan(spec, withFlagModel(args, flagModel), note, showOwnLoginNote)
	}
	if err != nil {
		return Plan{}, err
	}
	if spec.eng().NativeRegistry(ctx, target) {
		plan, err := registryPlan(ctx, spec, reg, target, note, withFlagModel(args, flagModel), o.PrintEnv)
		plan.Profile = o.Provider
		return plan, err
	}
	env, prefix, err := spec.Configure(ctx, target, "")
	if errors.Is(err, ErrOwnLogin) {
		// The agent's own login (opencode on Copilot) picks the model itself: plain launch, not a failure.
		return ownLoginPlan(spec, withFlagModel(args, flagModel), note, showOwnLoginNote)
	}
	if note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	if err != nil {
		return configureFailed(ctx, spec, name, o, args, err)
	}
	return finishPlan(spec, o, args, env, prefix)
}

// ownLoginPlan is the plain launch of an agent that signs in itself, printing note when show.
func ownLoginPlan(spec Schema, args []string, note string, show bool) (Plan, error) {
	if show {
		fmt.Fprintln(NoticeOut, note)
	}
	return plainPlan(spec, args)
}

// configureFailed offers configure once to a human at a terminal, then retries or falls back to a passthrough launch.
func configureFailed(ctx context.Context, spec Schema, name string, o LaunchOptions, args []string, err error) (Plan, error) {
	reason := fmt.Errorf("%w; %s", err, spec.CLI.ConfigureHint)
	var env, prefix []string
	// A human at a terminal is offered configure once; a yes re-resolves on the new state.
	if OfferConfigure != nil && !o.PrintEnv && OfferConfigure(name, reason) {
		env, prefix, err = retryConfigure(ctx, name, o)
	}
	if errors.Is(err, ErrOwnLogin) {
		return plainPlan(spec, args)
	}
	if err != nil {
		return passthrough(spec, name, reason, args)
	}
	return finishPlan(spec, o, args, env, prefix)
}

// finishPlan locates the agent binary and assembles the configured launch.
func finishPlan(spec Schema, o LaunchOptions, args, env, prefix []string) (Plan, error) {
	bin, err := lookPath(spec.CLI.Binary)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Binary: bin, Set: envMap(env), Argv: append(prefix, args...), Profile: o.Provider}, nil
}

// cliTarget resolves spec's target, then applies the model flag and pin to it.
func cliTarget(spec Schema, name string, reg *providers.Registry, sel providers.Selection, explicit bool, flagModel string) (providers.Target, string, error) {
	target, note, err := spec.ResolveTarget(reg, sel, explicit)
	if err == nil {
		var modelNote string
		if target, modelNote = agentModelTarget(name, target, sel, flagModel); modelNote != "" {
			note = strings.Join(slices.DeleteFunc([]string{note, modelNote}, func(s string) bool { return s == "" }), "; ")
		}
	}
	return target, note, err
}

// launchProfile resolves an agent CLI launch's profile: --profile > this workspace's last-used > the agent pin.
func launchProfile(name string, o LaunchOptions) (LaunchOptions, error) {
	return withAgentPins(name, withLastProfile(name, o))
}

// lastProfile is the profile this workspace last launched agentName with, "" unless it is registered and the agent can use it.
func lastProfile(reg *providers.Registry, agentName string) string {
	name := providers.LastProfile(agentName)
	p, err := reg.Get(name)
	if name == "" || err != nil {
		return ""
	}
	if agentName == "claude" && p.Auth.Login != "" || agentName != "claude" && !CanServe(agentName, p) {
		return ""
	}
	return name
}

// withLastProfile fills an unset --profile of an agent CLI launch from this workspace's last-used profile (before its pin).
func withLastProfile(name string, o LaunchOptions) LaunchOptions {
	if o.Provider != "" || (name != "copilot" && name != "opencode" && name != "codex" && name != "gemini") || !AgentActive(name) {
		return o
	}
	if reg, err := providers.Load(); err == nil {
		o.Provider = lastProfile(reg, name)
	}
	return o
}

// withAgentPins fills the profile the flags left empty from the agent's vault pin: --profile > pin > default.
// The model pin is applied after the target resolves (agentModelTarget).
func withAgentPins(name string, o LaunchOptions) (LaunchOptions, error) {
	if o.Provider == "" {
		pin, err := pinnedProfile(name)
		if err != nil {
			return LaunchOptions{}, err
		}
		o.Provider = pin
	}
	return o, nil
}

// pinnedProfile is agent name's vault-pinned registry profile; "" when unpinned or off, an error when the pin names no row.
func pinnedProfile(name string) (string, error) {
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
		return "", fmt.Errorf("%s: pinned profile %q is not a registry profile", name, pin.Profile)
	}
	if reg.Providers[pin.Profile] == nil {
		return "", nil // an own-login or PinIDs pin (opencode-go) launches on the agent's own plan
	}
	return pin.Profile, nil
}

// registryPlan is the interactive launch on a CLI's native model registry (copilot on Databricks). It fails closed:
// no error here reaches passthrough or the plain CLI, which would run on the CLI's own GitHub login.
func registryPlan(
	ctx context.Context, spec Schema, reg *providers.Registry, t providers.Target, note string, args []string,
	dry bool,
) (Plan, error) {
	if note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	cr, err := spec.eng().RegistryLaunch(ctx, reg, t, args, dry)
	if err != nil {
		return Plan{}, err
	}
	bin, err := lookPath(spec.CLI.Binary)
	if err != nil {
		cr.Revoke()
		return Plan{}, err
	}
	lenv, lprefix := spec.LaunchSpec()
	fmt.Fprintln(NoticeOut, cr.Banner)
	return Plan{
		Binary: bin, Set: envMap(append(lenv, cr.Env...)), Unset: cr.Unset, Warnings: cr.Warnings,
		Files:      cr.Files,
		Argv:       append(append(lprefix, cr.Argv...), cr.Args...),
		OnExecFail: cr.Revoke,
	}, nil
}

func retryConfigure(ctx context.Context, name string, o LaunchOptions) ([]string, []string, error) {
	reg, sel, err := selection(o)
	if err != nil {
		return nil, nil, err
	}
	spec := SchemaFor(name)
	target, note, err := spec.ResolveTarget(reg, sel, o.Provider != "")
	if err != nil {
		return nil, nil, err
	}
	if note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	return spec.Configure(ctx, target, "")
}

// authArgs reports an agent's own sign-in subcommand: codex/copilot login|logout, gemini/opencode auth.
func authArgs(args []string) bool {
	return len(args) > 0 && (args[0] == "login" || args[0] == "logout" || args[0] == "auth")
}

// WantsVersionWarning reports whether a launch of args is an interactive session, the only one that warns on a version.
func WantsVersionWarning(o LaunchOptions, args []string) bool {
	return !o.PrintEnv && !authArgs(args) && interactiveArgs(args)
}

func interactiveArgs(args []string) bool {
	if len(args) > 0 && args[0] == "run" {
		return false
	}
	for _, arg := range args {
		switch arg {
		case "--version", "-v", "-V", "--help", "-h":
			return false
		}
	}
	return true
}

// OfferConfigure is the interactive seam `configure` installs: asked once when an agent cannot be routed, it
// may set up that agent (login or key) and returns true when the launch should re-resolve. nil = no offer.
var OfferConfigure func(name string, reason error) bool

// passthrough launches the agent's own CLI untouched when the harness cannot route it: the user installed
// that CLI and it carries its own login, so refusing to run it would only hide its own `agy` sign-in.
func passthrough(spec Schema, name string, reason error, args []string) (Plan, error) {
	if _, err := lookPath(spec.CLI.Binary); err != nil {
		return Plan{}, reason // no CLI to fall back to: the routing error is the whole story
	}
	fmt.Fprintf(NoticeOut, "%v\nlaunching the plain %s CLI on its own login (not routed by claude-code)\n", reason, name)
	return plainPlan(spec, args)
}

// plainPlan is the agent's own CLI on its own login: no routing env, but the agent spec's launch env/args still apply.
func plainPlan(spec Schema, args []string) (Plan, error) {
	bin, err := lookPath(spec.CLI.Binary)
	if err != nil {
		return Plan{}, err
	}
	env, prefix := spec.LaunchSpec()
	return Plan{Binary: bin, Set: envMap(env), Argv: append(prefix, args...)}, nil
}

// Credentials ride these env vars (never argv); the config text names the variable, not the secret.
const (
	OpenCodeKeyEnv = "HARNESS_OPENCODE_API_KEY"
)

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
	env = append(env, s.eng().LaunchEnv()...)
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
func (s Schema) LaunchSpec() (env, argv []string) { return s.applyLaunch(nil, nil, true) }

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

// Router seams (tests stub these so no daemon is spawned).
var (
	ensureRouter = proxy.Ensure
	routerSecret = proxy.ClientSecret
	mintSession  = proxy.MintSessionToken
)

// StubRouter points configure at base: an unstubbed Ensure would relaunch the test binary as the daemon.
// Tests call it directly; restore undoes it.
func StubRouter(base string) (restore func()) {
	oe := ensureRouter
	ensureRouter = func(context.Context) (string, error) { return base, nil }
	return func() { ensureRouter = oe }
}

// StubProxyForTest stubs the router, its secret and session mints for a whole test binary (call from TestMain),
// and shortens the dispatch limiter's poll; no proxy daemon is reached or spawned.
func StubProxyForTest(base, secret string) {
	ensureRouter = func(context.Context) (string, error) { return base, nil }
	routerSecret = func() (string, error) { return secret, nil }
	mintSession = func(context.Context, string, int) (string, error) { return "", errors.New("session mint is stubbed") }
	retry.SharedPoll = 25 * time.Millisecond
}

type directKey struct{}

// WithDirect makes configure skip the router and hand the agent the provider's own route and credential.
func WithDirect(ctx context.Context) context.Context {
	return context.WithValue(ctx, directKey{}, true)
}

func IsDirect(ctx context.Context) bool { v, _ := ctx.Value(directKey{}).(bool); return v }

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

// Positional guards a dash-led prompt from flag parsing.
func Positional(prompt string) []string {
	if strings.HasPrefix(prompt, "-") {
		return []string{"--", prompt}
	}
	return []string{prompt}
}

// login runs the agent on its own subscription sign-in; a login provider serves only the CLI it names.
func login(e Driver, p *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	if !e.LoginServes(p) {
		return nil, nil, fmt.Errorf("%s: provider %s is the %s sign-in; use claude-code run %s --provider %s", e.Name(), p.Name, p.Auth.Login, p.Auth.Login, p.Name)
	}
	return e.Login(p, model, h)
}

// HeadlessLaunch applies name's spec Launch.Env (never Launch.Args) to argv, with the route env going last.
func HeadlessLaunch(name string, env, argv []string) (launchedEnv, launchedArgv []string) {
	launchEnv, argv := SchemaFor(name).applyLaunch(nil, argv, false)
	return routeEnvLast(launchEnv, env), argv
}
