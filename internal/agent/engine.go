package agent

import (
	"context"

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

// LoginStatus reports whether name's own login is present and what it is; a name with no engine has none.
func LoginStatus(name string) (present bool, source string) {
	if e, ok := engines[name]; ok {
		return e.LoginStatus()
	}
	return false, ""
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
