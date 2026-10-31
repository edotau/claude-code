package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// engine is the per-CLI behaviour behind vendorRunner and configure; every external agent has one.
type engine interface {
	// Name is the roster agent the engine drives.
	Name() string
	// attach folds --attach into the prompt for a CLI with no file-list argv.
	attach(prompt string, paths []string, tools bool) (string, error)
	// before adjusts the target configure sees and prints any pre-run notice.
	before(req RunOptions, t providers.Target) providers.Target
	// after applies per-CLI options to the configured env and argv.
	after(p *vendorPrep, req RunOptions)
	// ranModel is the model the CLI runs on t, as reported in the Result.
	ranModel(t providers.Target) string
	// loginServes reports whether p's sign-in is one this CLI can run on.
	loginServes(p *providers.Provider) bool
	// login wires the CLI's own subscription sign-in.
	login(p *providers.Provider, model string, h *Headless) (env, argv []string, err error)
	// wire builds env and argv for a resolved harness route.
	wire(ctx context.Context, r wireRoute, h *Headless) (env, argv []string, err error)
	// fallbackTarget is the CLI's own route to a working credential when the session provider has none.
	fallbackTarget(reg *providers.Registry) (providers.Target, string, bool)
	// describeRoute reports the fallback leg the CLI resolves to today.
	describeRoute(reg *providers.Registry) RouteInfo
	// LoginStatus reports whether the CLI's own login is present and what it is.
	LoginStatus() (present bool, source string)
	// preRoute resolves the target ahead of the session provider; ok reports the CLI decided (err may be ErrOwnLogin).
	preRoute(reg *providers.Registry, explicit bool) (t providers.Target, note string, ok bool, err error)
	// gate vets a target the session or the caller named, returning the target to run and a notice.
	gate(reg *providers.Registry, t providers.Target) (providers.Target, string, error)
	// gateExplicit is gate for a caller-named provider or model, plus the CLI's own quota and tier handling.
	gateExplicit(req RunOptions, reg *providers.Registry, t providers.Target) (providers.Target, string, error)
	// noRoute is the error when neither the session nor the CLI's fallback chain resolved a target; nil keeps t.
	noRoute(reg *providers.Registry, t providers.Target) error
	// fallbackNote is the notice that replaces the generic one for fallback target ft; ok is false for the generic text.
	fallbackNote(ft providers.Target, note string) (string, bool)
	// followsPin reports whether the CLI runs on session provider p.
	followsPin(p *providers.Provider) bool
	// launchEnv is the env every launch of the CLI carries ahead of the vendor spec's.
	launchEnv() []string
	// presence is the CLI-specific no-network readiness check for a routed cell.
	presence(c Cell) error
	// ownLogin is the headless form of the CLI running on its own stored login.
	ownLogin(h Headless) (env, argv []string, err error)
	// checkLogin probes the CLI's own sign-in for the provider sign-in rows.
	checkLogin(ctx context.Context) (providers.AuthVerdict, string)
	// render writes the CLI's config files and reports drift.
	render(reg *providers.Registry, sel providers.Selection, home string, o RenderOptions, out io.Writer) (bool, error)
	// configPaths lists the files render writes, for revert.
	configPaths(home string) ([]labelPath, error)
	// launchArgs applies the -f file to the launch argv.
	launchArgs(file string, args []string) ([]string, error)
	// profileHint is the error a failed selection resolves to when the CLI knows the cause.
	profileHint(o LaunchOptions) error
	// nativeRegistry reports whether t launches on the CLI's own model registry.
	nativeRegistry(ctx context.Context, t providers.Target) bool
	// routeDetail is the "<agent> route" doctor detail; ok is false when there is none.
	routeDetail() (detail string, ok bool)
}

// headlessEngine is an engine vendorRunner can exec; copilot runs through copilotRunner instead.
type headlessEngine interface {
	engine
	// exec runs the prepared child and fills the Result.
	exec(ctx context.Context, p vendorPrep, req RunOptions, sink Sink) (Result, error)
}

// engines holds every external CLI's engine by agent name.
var engines = map[string]engine{
	"codex":    codexEngine{baseEngine{"codex"}},
	"gemini":   geminiEngine{baseEngine{"gemini"}},
	"opencode": opencodeEngine{baseEngine{"opencode"}},
	"copilot":  copilotEngine{baseEngine{"copilot"}},
}

// eng is the engine driving s; a row with none (claude) gets baseEngine's defaults.
func (s Schema) eng() engine {
	if e, ok := engines[s.Name]; ok {
		return e
	}
	return baseEngine{s.Name}
}

// schemaFor is the roster row for name, or a bare row carrying only the name when the roster has none.
func schemaFor(name string) Schema {
	if s, ok := LookupSchema(name); ok {
		return s
	}
	return Schema{Name: name}
}

// LoginStatus reports whether the agent's own login is present and what it is; a row with no engine has none.
func (s Schema) LoginStatus() (present bool, source string) { return s.eng().LoginStatus() }

// loginPresent is LoginStatus's presence bit; an engine whose label is costly (copilot) answers it itself.
func (s Schema) loginPresent() bool {
	e := s.eng()
	if lp, ok := e.(interface{ loginPresent() bool }); ok {
		return lp.loginPresent()
	}
	present, _ := e.LoginStatus()
	return present
}

// wireRoute is the route configure resolved; Target keeps the session model for the [1m] decision.
type wireRoute struct {
	Target                     providers.Target
	Provider                   *providers.Provider
	Dialect, Base, Model, Cred string
	Routed                     bool
}

// routeFallback is the target ri names, with ri's reason as the fallback note.
func routeFallback(reg *providers.Registry, ri RouteInfo) (providers.Target, string, bool) {
	if t, ok := routeTarget(reg, ri); ok {
		return t, ri.Reason, true
	}
	return providers.Target{}, "", false
}

// baseEngine holds the defaults a CLI overrides only where it differs.
type baseEngine struct{ name string }

func (b baseEngine) Name() string { return b.name }

// bin is the CLI's executable, as a fatal Error when it is not installed.
func (b baseEngine) bin() (string, error) {
	s, _ := LookupSchema(b.name)
	bin, err := paths.VendorBin(s.CLI.Binary)
	if err != nil {
		return "", &Error{Kind: KindFatal, Agent: b.name, Err: err}
	}
	return bin, nil
}

func (baseEngine) attach(prompt string, _ []string, _ bool) (string, error) { return prompt, nil }

func (baseEngine) before(_ RunOptions, t providers.Target) providers.Target { return t }

func (baseEngine) after(*vendorPrep, RunOptions) {}

func (baseEngine) ranModel(t providers.Target) string { return t.Model }

func (b baseEngine) loginServes(p *providers.Provider) bool { return p.Auth.Login == b.name }

func (b baseEngine) describeRoute(*providers.Registry) RouteInfo {
	return RouteInfo{Agent: b.name, Leg: LegNone, Reason: "no fallback chain defined for " + b.name}
}

func (baseEngine) LoginStatus() (bool, string) { return false, "" }

func (baseEngine) preRoute(*providers.Registry, bool) (providers.Target, string, bool, error) {
	return providers.Target{}, "", false, nil
}

func (baseEngine) gate(_ *providers.Registry, t providers.Target) (providers.Target, string, error) {
	return t, "", nil
}

func (baseEngine) gateExplicit(_ RunOptions, _ *providers.Registry, t providers.Target) (providers.Target, string, error) {
	return t, "", nil
}

func (baseEngine) noRoute(*providers.Registry, providers.Target) error { return nil }

func (baseEngine) fallbackNote(providers.Target, string) (string, bool) { return "", false }

func (baseEngine) followsPin(*providers.Provider) bool { return true }

func (baseEngine) launchEnv() []string { return nil }

func (baseEngine) presence(Cell) error { return nil }

func (b baseEngine) ownLogin(h Headless) ([]string, []string, error) {
	if err := checkPrompt(h); err != nil {
		return nil, nil, err
	}
	return nil, nil, fmt.Errorf("%s: no headless form on its own login; pass --provider", b.name)
}

// checkPrompt rejects a headless run with no prompt.
func checkPrompt(h Headless) error {
	if strings.TrimSpace(h.Prompt) == "" {
		return errors.New("agents: empty prompt")
	}
	return nil
}

func (b baseEngine) checkLogin(context.Context) (providers.AuthVerdict, string) {
	return providers.VerdictTransient, "unknown sign-in " + b.name
}

func (baseEngine) render(*providers.Registry, providers.Selection, string, RenderOptions, io.Writer) (bool, error) {
	return false, nil
}

func (baseEngine) configPaths(string) ([]labelPath, error) { return nil, nil }

func (b baseEngine) launchArgs(file string, args []string) ([]string, error) {
	if file != "" {
		return nil, fmt.Errorf("-f is codex only (got %s)", b.name)
	}
	return args, nil
}

func (baseEngine) profileHint(LaunchOptions) error { return nil }

func (baseEngine) nativeRegistry(context.Context, providers.Target) bool { return false }

func (baseEngine) routeDetail() (string, bool) { return "", false }

func (b baseEngine) login(*providers.Provider, string, *Headless) ([]string, []string, error) {
	return nil, nil, errUnknownAgent(b.name)
}

func (b baseEngine) wire(context.Context, wireRoute, *Headless) ([]string, []string, error) {
	return nil, nil, errUnknownAgent(b.name)
}

func (baseEngine) fallbackTarget(*providers.Registry) (providers.Target, string, bool) {
	return providers.Target{}, "", false
}
