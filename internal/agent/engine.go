package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"

	"github.com/edotau/claude-code/internal/models"
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
	// Render writes the CLI's config files and reports drift.
	Render(reg *providers.Registry, sel providers.Selection, home string, o agentconfig.RenderOptions, out io.Writer) (bool, error)
	// ConfigPaths lists the files render writes, for revert.
	ConfigPaths(home string) ([]agentconfig.LabelPath, error)
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
	// RenderSettings applies s to a built launch; emulated names what a shim honoured; cleanup is never nil.
	RenderSettings(ctx context.Context, s Settings, env, argv []string) (outEnv, outArgv, emulated []string, cleanup func(), err error)
}

// HeadlessDriver is an engine cliEngine can exec; copilotEngine is its own Engine instead.
type HeadlessDriver interface {
	Driver
	// Exec runs the prepared child and fills the Result.
	Exec(ctx context.Context, p CLIPrep, req RunOptions, sink wire.Sink) (wire.Result, error)
}

// Eng is the engine driving s; a row with none (claude) gets baseEngine's defaults.
func (s Schema) Eng() Driver {
	if e, ok := driverFor(s.Name); ok {
		return e
	}
	return BaseEngine{s.Name}
}

// SchemaFor is the roster row for name, or a bare row carrying only the name when the roster has none.
func SchemaFor(name string) Schema {
	if s, ok := LookupSchema(name); ok {
		return s
	}
	return Schema{Name: name}
}

// ProbeTimeout bounds a vendor CLI's login probe; a var so tests stay fast.
var ProbeTimeout = 8 * time.Second

// RegistryLaunch is a CLI's native model-registry launch: env to set and unset, argv, args and the token to revoke.
type RegistryLaunch struct {
	Env, Unset, Argv, Args, Warnings []string
	Files                            []LaunchFile
	Revoke                           func()
	Banner                           string
}

// LoginPresent is the agent's own sign-in presence bit (no network).
func (s Schema) LoginPresent() bool { return AuthStatus(s.Name).Present }

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

func (b BaseEngine) Caps() Caps {
	s, _ := lookupRow(b.name)
	return s.CLI.Caps
}

func (b BaseEngine) Available() error {
	_, err := paths.AgentBin(SchemaFor(b.name).CLI.Binary)
	return err
}

// Bin is the CLI's executable, as a fatal Error when it is not installed.
func (b BaseEngine) Bin() (string, error) {
	s, _ := LookupSchema(b.name)
	bin, err := paths.AgentBin(s.CLI.Binary)
	if err != nil {
		return "", retry.Fatal(b.name, err)
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

func (BaseEngine) Render(*providers.Registry, providers.Selection, string, agentconfig.RenderOptions, io.Writer) (bool, error) {
	return false, nil
}

func (BaseEngine) ConfigPaths(string) ([]agentconfig.LabelPath, error) { return nil, nil }

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

// RenderSettings appends the CLI's LaunchEnv and Record.Launch env (route keys win) and, interactive only, its args.
func (b BaseEngine) RenderSettings(_ context.Context, s Settings, env, argv []string) ([]string, []string, []string, func(), error) {
	launchEnv := SchemaFor(b.name).Eng().LaunchEnv()
	if l := s.Record.Launch; l != nil {
		launchEnv = append(launchEnv, l.Env...)
		if !s.Headless {
			argv = append(argv, l.Args...)
		}
	}
	return RouteEnvLast(launchEnv, env), argv, nil, func() {}, nil
}

func (b BaseEngine) Login(*providers.Provider, string, *Headless) ([]string, []string, error) {
	return nil, nil, errUnknownAgent(b.name)
}

func (b BaseEngine) Wire(context.Context, WireRoute, *Headless) ([]string, []string, error) {
	return nil, nil, errUnknownAgent(b.name)
}

func (BaseEngine) FallbackTarget(*providers.Registry) (providers.Target, string, bool) {
	return providers.Target{}, "", false
}

// LiveBackend is how one agent holds a session open: build the process, write a turn, parse one turn's events.
type LiveBackend interface {
	// Build returns the unstarted child and the provider profile it routes through.
	Build(ctx context.Context, req RunOptions) (cmd *exec.Cmd, tail *retry.TailBuffer, provider string, err error)
	WriteTurn(w io.Writer, prompt string) error
	// Parse reads dec up to and including the turn's result event; usage and cost are that turn's alone.
	Parse(dec *bufio.Reader, sink wire.Sink, lg func(kind, text string)) (wire.Result, error)
}

// LiveCapable is a CLI that can hold a session open; a fresh backend serves each session.
type LiveCapable interface{ NewLive() LiveBackend }

// Live asserts on the CLI behind the agent: the engine itself, or a cliEngine's engine.
func (s Schema) Live() (LiveCapable, bool) {
	r, err := Lookup(s.Name)
	if err != nil {
		return nil, false
	}
	var cli any = r
	if v, ok := r.(cliEngine); ok {
		cli = v.HeadlessDriver
	}
	lc, ok := cli.(LiveCapable)
	return lc, ok
}

// HasLive reports whether the agent can hold a live session.
func (s Schema) HasLive() bool {
	_, ok := s.Live()
	return ok
}

// WriteJSONLine writes v as one NDJSON line.
func WriteJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// The helpers below are the vendor packages' (claude, codex, gemini) reach into the dispatch internals.

// ChildErr names a failed child by its stderr tail; a deadline surfaces as ctx.Err.
func ChildErr(ctx context.Context, name string, err error, tail *retry.TailBuffer) error {
	return childErr(ctx, name, err, tail)
}

// StatusOf maps a classified failure to the envelope status.
func StatusOf(e *retry.Error) string { return statusOf(e) }

// IsResultError reports a failed turn whose result event was consumed, so the stream sits at a turn boundary.
func IsResultError(err error) bool {
	_, ok := errors.AsType[wire.ResultError](err)
	return ok
}

// ResolveRequestTarget is the registry and target req's provider and model resolve to.
func ResolveRequestTarget(req RunOptions) (*providers.Registry, providers.Target, error) {
	return resolveTarget(req)
}

// PersonaArgs is the claude argv for a persona.
func PersonaArgs(p *Schema) []string { return personaArgs(p) }

// SessionProvider is the session profile agentName runs on.
func SessionProvider(agentName string) string { return sessionProvider(agentName) }

// CustomEnv is the vault env bucket resolved, as a launch layers it.
func CustomEnv() map[string]string { return customEnv() }

// Dispatch names a RouteInfo's resolved credential path. Why: Anthropic's terms forbid subscription OAuth
// in third-party tools, so neither chain below ever offers the Claude subscription (passthrough, no
// login) as a dispatch.
const (
	RouteCopilotLogin  = "copilot-login"
	RouteOwnLogin      = "own-login"
	RouteByokAnthropic = "byok-anthropic"
	RouteDatabricks    = "databricks"
	RouteNone          = "none"
)

// RouteInfo is the agent dispatch `claude-code configure github`, `generate` and `doctor` report: which
// credential path an agent CLI runs on today, and why. OK is false only for RouteNone.
type RouteInfo struct {
	Agent, Route, Provider, Model, SmallModel, Reason string
	OK                                                bool
}

// DescribeRoute reports the fallback dispatch the agent (opencode|copilot) resolves to today, in priority
// order. Every other agent reports RouteNone with a plain explanation — they have no fallback chain.
func (s Schema) DescribeRoute(reg *providers.Registry) RouteInfo { return s.Eng().DescribeRoute(reg) }

// RouteTarget converts a resolved RouteInfo (OK and naming a registered provider) into a Target;
// ok is false for RouteNone or a provider DescribeRoute named that the registry no longer has.
func RouteTarget(reg *providers.Registry, ri RouteInfo) (providers.Target, bool) {
	if !ri.OK || ri.Provider == "" {
		return providers.Target{}, false
	}
	p, ok := reg.Providers[ri.Provider]
	if !ok {
		return providers.Target{}, false
	}
	return providers.Target{Provider: p, Model: ri.Model}, true
}

// RouteErr is ri's reason as an error when ri resolved no route, else nil.
func RouteErr(ri RouteInfo) error {
	if !ri.OK {
		return errors.New(ri.Reason)
	}
	return nil
}

// RouteDetail returns the "<agent> route" detail string DoctorChecks would emit for the agent, without
// running the rest of the doctor sweep; ok is false for an unknown agent or a registry load error.
func RouteDetail(name string) (detail string, ok bool) {
	return SchemaFor(name).Eng().RouteDetail()
}

// LoadSelection is the registry and its unpinned selection; ok is false when the registry does not load.
func LoadSelection() (reg *providers.Registry, sel providers.Selection, selErr error, ok bool) {
	reg, err := providers.Load()
	if err != nil || reg == nil {
		return nil, providers.Selection{}, nil, false
	}
	sel, selErr = providers.Select(reg, "")
	return reg, sel, selErr, true
}

// ResolveTarget picks the target an agent launch uses: sel's target when it can serve the agent
// (a login provider naming it, or a route on one of its dialects), else — only when the caller named
// no provider itself (explicit is false) — the agent's own subscription login or first keyed
// provider with a resolvable credential. note is a one-line explanation to print on fallback.
func (s Schema) ResolveTarget(reg *providers.Registry, sel providers.Selection, explicit bool) (target providers.Target, note string, err error) {
	name, e := s.Name, s.Eng()
	t, ok := sel.Aliases[models.Opus]
	if !ok {
		return providers.Target{}, "", fmt.Errorf("%s: no opus target resolved", name)
	}
	// A CLI's own login (unrelated to the session provider) is preferred
	// whenever it is present, ahead of a session provider that could serve it; an explicit --provider still wins.
	if pt, pnote, ok, perr := e.PreRoute(reg, explicit); ok {
		return pt, pnote, perr
	}
	if explicit || (CanServe(name, t.Provider) && e.FollowsPin(t.Provider)) {
		return e.Gate(reg, t)
	}
	if ft, fallbackNote, ok := e.FallbackTarget(reg); ok {
		note = fmt.Sprintf("%s: session provider %s cannot serve %s; using %s", name, t.Provider.Name, name, fallbackNote)
		if own, ok := e.FallbackNote(ft, fallbackNote); ok {
			note = own
		}
		if ft.Provider == nil {
			return ft, note, ErrOwnLogin // the agent's own stored login: launch it plain, nothing to inject
		}
		return ft, note, nil
	}
	// opencode and copilot have a defined fallback chain (DescribeRoute); nothing in it resolved either.
	if err := e.NoRoute(reg, t); err != nil {
		return providers.Target{}, "", err
	}
	return t, "", nil
}

// ErrOwnLogin means the agent should run on the credentials its own CLI stores (no harness route injected).
var ErrOwnLogin = errors.New("agent runs on its own login")

// HubEngine is one hub ladder step: an Engine that names the provider account answering it, so every dispatch of a
// chain reads the same Result.Agent and only Result.Provider shows which account did.
type HubEngine interface {
	Engine
	HubProvider() *providers.Provider
	// WithFirstOutput returns the step failing with ErrStalled when nothing streams within d.
	WithFirstOutput(d time.Duration) Engine
}

// errNoHub is returned when a hub route is asked for but no linked vendor registered the opencode Hub.
var errNoHub = errors.New("no opencode hub registered: the opencode agent is not linked into this binary")

// Hub is the opencode-routed dispatch ladder, registered by internal/opencode.
type Hub struct {
	// Start resolves the provider a hub agent's first dispatch targets (explicitProvider forces one).
	Start func(reg *providers.Registry, agentName, explicitProvider string) (*providers.Provider, error)
	// Dispatch builds the hub step for agentName on provider; reg resolves a provider:model override.
	Dispatch func(agentName string, provider *providers.Provider, reg *providers.Registry) HubEngine
	// OwnPlan reports whether an opencodeID is opencode's own plan (no registry provider, billed as a subscription).
	OwnPlan func(opencodeID string) bool
}

var hub *Hub

// IsHubAgent reports whether name is one ask routes through opencode unless --native is set. codex and
// copilot run their own CLI first (user decision): the hub cannot pass copilot's `auto` or its pinned account.
func IsHubAgent(name string) bool {
	s, ok := lookupRow(name)
	return ok && s.CLI.Hub
}

// hubLinked reports whether a linked vendor registered the opencode Hub.
func hubLinked() bool { return hub != nil }

// HubStart resolves the provider a hub agent's first dispatch targets.
func HubStart(reg *providers.Registry, agentName, explicitProvider string) (*providers.Provider, error) {
	if hub == nil {
		return nil, errNoHub
	}
	return hub.Start(reg, agentName, explicitProvider)
}

// NewHubDispatch builds a hub dispatch Agent for agentName on provider; reg resolves a provider:model override.
func NewHubDispatch(agentName string, provider *providers.Provider, reg *providers.Registry) Engine {
	return hub.Dispatch(agentName, provider, reg)
}

// hubDispatchFor maps an opencode cell "<opencodeID>/<model>" to the hub dispatch on that provider and the bare model.
func hubDispatchFor(reg *providers.Registry, cell string) (Engine, string, error) {
	id, model, ok := strings.Cut(cell, "/")
	if !ok || reg == nil {
		return nil, "", fmt.Errorf("cell %q is not <opencodeID>/<model>", cell)
	}
	if hub == nil {
		return nil, "", errNoHub
	}
	var first *providers.Provider
	for _, name := range reg.Names() {
		p := reg.Providers[name]
		if p.OpencodeID != id {
			continue
		}
		if providers.CredentialPresent(p) { // rows can share an opencodeID (chatgpt, openai): the signed-in one serves it
			return hub.Dispatch("opencode", p, reg), model, nil
		}
		if first == nil {
			first = p
		}
	}
	if first != nil {
		return hub.Dispatch("opencode", first, reg), model, nil
	}
	// opencode's own plans (opencode-go, opencode) have no registry provider: opencode resolves them itself.
	if hub.OwnPlan(id) {
		return hub.Dispatch("opencode", &providers.Provider{Name: id, OpencodeID: id}, reg), model, nil
	}
	return nil, "", fmt.Errorf("no provider has opencodeID %q", id)
}
