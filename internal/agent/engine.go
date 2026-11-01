package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Driver is the per-CLI behaviour behind cliEngine and configure; a vendor registers one with Register.
type Driver interface {
	// Name is the roster agent the engine drives.
	Name() string
	// Caps is the roster row's verified flags; with Name and Available it is every Engine's shared base.
	Caps() Caps
	// Available reports whether the CLI's binary is installed.
	Available() error
	// Attach folds --attach into the prompt for a CLI with no file-list argv.
	Attach(prompt string, paths []string, tools bool) (string, error)
	// Before adjusts the target configure sees and prints any pre-run notice.
	Before(req RunOptions, t providers.Target) providers.Target
	// After applies per-CLI options to the configured env and argv.
	After(p *CLIPrep, req RunOptions)
	// RanModel is the model the CLI runs on t, as reported in the Result.
	RanModel(t providers.Target) string
	// LoginServes reports whether p's sign-in is one this CLI can run on.
	LoginServes(p *providers.Provider) bool
	// Login wires the CLI's own subscription sign-in.
	Login(p *providers.Provider, model string, h *Headless) (env, argv []string, err error)
	// Wire builds env and argv for a resolved harness route.
	Wire(ctx context.Context, r WireRoute, h *Headless) (env, argv []string, err error)
	// FallbackTarget is the CLI's own route to a working credential when the session provider has none.
	FallbackTarget(reg *providers.Registry) (providers.Target, string, bool)
	// DescribeRoute reports the fallback dispatch the CLI resolves to today.
	DescribeRoute(reg *providers.Registry) RouteInfo
	// LoginStatus reports whether the CLI's own login is present and what it is.
	LoginStatus() (present bool, source string)
	// PreRoute resolves the target ahead of the session provider; ok reports the CLI decided (err may be ErrOwnLogin).
	PreRoute(reg *providers.Registry, explicit bool) (t providers.Target, note string, ok bool, err error)
	// Gate vets a target the session or the caller named, returning the target to run and a notice.
	Gate(reg *providers.Registry, t providers.Target) (providers.Target, string, error)
	// GateExplicit is gate for a caller-named provider or model, plus the CLI's own quota and tier handling.
	GateExplicit(req RunOptions, reg *providers.Registry, t providers.Target) (providers.Target, string, error)
	// NoRoute is the error when neither the session nor the CLI's fallback chain resolved a target; nil keeps t.
	NoRoute(reg *providers.Registry, t providers.Target) error
	// FallbackNote is the notice that replaces the generic one for fallback target ft; ok is false for the generic text.
	FallbackNote(ft providers.Target, note string) (string, bool)
	// FollowsPin reports whether the CLI runs on session provider p.
	FollowsPin(p *providers.Provider) bool
	// LaunchEnv is the env every launch of the CLI carries ahead of the agent spec's.
	LaunchEnv() []string
	// Presence is the CLI-specific no-network readiness check for a routed cell.
	Presence(c router.Cell) error
	// OwnLogin is the headless form of the CLI running on its own stored login.
	OwnLogin(h Headless) (env, argv []string, err error)
	// CheckLogin probes the CLI's own sign-in for the provider sign-in rows.
	CheckLogin(ctx context.Context) (providers.AuthVerdict, string)
	// Render writes the CLI's config files and reports drift.
	Render(reg *providers.Registry, sel providers.Selection, home string, o agentrender.RenderOptions, out io.Writer) (bool, error)
	// ConfigPaths lists the files render writes, for revert.
	ConfigPaths(home string) ([]agentrender.LabelPath, error)
	// LaunchArgs applies the -f file to the launch argv.
	LaunchArgs(file string, args []string) ([]string, error)
	// ProfileHint is the error a failed selection resolves to when the CLI knows the cause.
	ProfileHint(o LaunchOptions) error
	// NativeRegistry reports whether t launches on the CLI's own model registry.
	NativeRegistry(ctx context.Context, t providers.Target) bool
	// RegistryLaunch builds the native-registry launch NativeRegistry reported; dry mints and writes nothing.
	RegistryLaunch(ctx context.Context, reg *providers.Registry, t providers.Target, args []string, dry bool) (RegistryLaunch, error)
	// RouteDetail is the "<agent> route" doctor detail; ok is false when there is none.
	RouteDetail() (detail string, ok bool)
}

// HeadlessDriver is an engine cliEngine can exec; copilotEngine is its own Engine instead.
type HeadlessDriver interface {
	Driver
	// Exec runs the prepared child and fills the Result.
	Exec(ctx context.Context, p CLIPrep, req RunOptions, sink Sink) (Result, error)
}

// eng is the engine driving s; a row with none (claude) gets baseEngine's defaults.
func (s Schema) eng() Driver {
	if e, ok := driverFor(s.Name); ok {
		return e
	}
	return BaseEngine{s.Name}
}

// schemaFor is the roster row for name, or a bare row carrying only the name when the roster has none.
func schemaFor(name string) Schema {
	if s, ok := LookupSchema(name); ok {
		return s
	}
	return Schema{Name: name}
}

// CheckLogin probes name's own CLI sign-in for the provider sign-in rows.
func CheckLogin(ctx context.Context, name string) (providers.AuthVerdict, string) {
	return schemaFor(name).eng().CheckLogin(ctx)
}

// ProbeTimeout bounds a vendor CLI's login probe; a var so tests stay fast.
var ProbeTimeout = 8 * time.Second

// LoginStatus reports whether the agent's own login is present and what it is; a row with no engine has none.
func (s Schema) LoginStatus() (present bool, source string) { return s.eng().LoginStatus() }

// RegistryLaunch is a CLI's native model-registry launch: env to set and unset, argv, args and the token to revoke.
type RegistryLaunch struct {
	Env, Unset, Argv, Args, Warnings []string
	Revoke                           func()
	Banner                           string
}

// SchemaFor is schemaFor for the vendor packages.
func SchemaFor(name string) Schema { return schemaFor(name) }

// LoginPresent is LoginStatus's presence bit; an engine whose label is costly (copilot) answers it with LoginPresent.
func (s Schema) LoginPresent() bool {
	e := s.eng()
	if lp, ok := e.(interface{ LoginPresent() bool }); ok {
		return lp.LoginPresent()
	}
	present, _ := e.LoginStatus()
	return present
}

// WireRoute is the route configure resolved; Target keeps the session model for the [1m] decision.
type WireRoute struct {
	Target                     providers.Target
	Provider                   *providers.Provider
	Dialect, Base, Model, Cred string
	Routed                     bool
}

// RouteFallback is the target ri names, with ri's reason as the fallback note.
func RouteFallback(reg *providers.Registry, ri RouteInfo) (providers.Target, string, bool) {
	if t, ok := RouteTarget(reg, ri); ok {
		return t, ri.Reason, true
	}
	return providers.Target{}, "", false
}

// BaseEngine holds the defaults a CLI overrides only where it differs.
type BaseEngine struct{ name string }

// NewBaseEngine is the defaults for the roster agent name; a vendor Driver embeds it.
func NewBaseEngine(name string) BaseEngine { return BaseEngine{name} }

func (b BaseEngine) Name() string { return b.name }

func (b BaseEngine) Caps() Caps { return schemaFor(b.name).CLI.Caps }

func (b BaseEngine) Available() error {
	_, err := paths.AgentBin(schemaFor(b.name).CLI.Binary)
	return err
}

// Bin is the CLI's executable, as a fatal Error when it is not installed.
func (b BaseEngine) Bin() (string, error) {
	s, _ := LookupSchema(b.name)
	bin, err := paths.AgentBin(s.CLI.Binary)
	if err != nil {
		return "", &Error{Kind: KindFatal, Agent: b.name, Err: err}
	}
	return bin, nil
}

func (BaseEngine) Attach(prompt string, _ []string, _ bool) (string, error) { return prompt, nil }

func (BaseEngine) Before(_ RunOptions, t providers.Target) providers.Target { return t }

func (BaseEngine) After(*CLIPrep, RunOptions) {}

func (BaseEngine) RanModel(t providers.Target) string { return t.Model }

func (b BaseEngine) LoginServes(p *providers.Provider) bool { return p.Auth.Login == b.name }

func (b BaseEngine) DescribeRoute(*providers.Registry) RouteInfo {
	return RouteInfo{Agent: b.name, Route: RouteNone, Reason: "no fallback chain defined for " + b.name}
}

func (BaseEngine) LoginStatus() (bool, string) { return false, "" }

func (BaseEngine) PreRoute(*providers.Registry, bool) (providers.Target, string, bool, error) {
	return providers.Target{}, "", false, nil
}

func (BaseEngine) Gate(_ *providers.Registry, t providers.Target) (providers.Target, string, error) {
	return t, "", nil
}

func (BaseEngine) GateExplicit(_ RunOptions, _ *providers.Registry, t providers.Target) (providers.Target, string, error) {
	return t, "", nil
}

func (BaseEngine) NoRoute(*providers.Registry, providers.Target) error { return nil }

func (BaseEngine) FallbackNote(providers.Target, string) (string, bool) { return "", false }

func (BaseEngine) FollowsPin(*providers.Provider) bool { return true }

func (BaseEngine) LaunchEnv() []string { return nil }

func (BaseEngine) Presence(router.Cell) error { return nil }

func (b BaseEngine) OwnLogin(h Headless) ([]string, []string, error) {
	if err := CheckPrompt(h); err != nil {
		return nil, nil, err
	}
	return nil, nil, fmt.Errorf("%s: no headless form on its own login; pass --provider", b.name)
}

// CheckPrompt rejects a headless run with no prompt.
func CheckPrompt(h Headless) error {
	if strings.TrimSpace(h.Prompt) == "" {
		return errors.New("agents: empty prompt")
	}
	return nil
}

func (b BaseEngine) CheckLogin(context.Context) (providers.AuthVerdict, string) {
	return providers.VerdictTransient, "unknown sign-in " + b.name
}

func (BaseEngine) Render(*providers.Registry, providers.Selection, string, agentrender.RenderOptions, io.Writer) (bool, error) {
	return false, nil
}

func (BaseEngine) ConfigPaths(string) ([]agentrender.LabelPath, error) { return nil, nil }

func (b BaseEngine) LaunchArgs(file string, args []string) ([]string, error) {
	if file != "" {
		return nil, fmt.Errorf("-f is codex only (got %s)", b.name)
	}
	return args, nil
}

func (BaseEngine) ProfileHint(LaunchOptions) error { return nil }

func (BaseEngine) NativeRegistry(context.Context, providers.Target) bool { return false }

func (b BaseEngine) RegistryLaunch(context.Context, *providers.Registry, providers.Target, []string, bool) (RegistryLaunch, error) {
	return RegistryLaunch{}, fmt.Errorf("%s has no native model registry", b.name)
}

func (BaseEngine) RouteDetail() (string, bool) { return "", false }

func (b BaseEngine) Login(*providers.Provider, string, *Headless) ([]string, []string, error) {
	return nil, nil, errUnknownAgent(b.name)
}

func (b BaseEngine) Wire(context.Context, WireRoute, *Headless) ([]string, []string, error) {
	return nil, nil, errUnknownAgent(b.name)
}

func (BaseEngine) FallbackTarget(*providers.Registry) (providers.Target, string, bool) {
	return providers.Target{}, "", false
}
