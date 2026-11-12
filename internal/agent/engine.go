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

// Eng is the engine driving s; a row with none (claude) gets BaseEngine's defaults.
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

// The helpers below are the vendor packages' reach into the dispatch internals.

// IsResultError reports a failed turn whose result event was consumed, so the stream sits at a turn boundary.
func IsResultError(err error) bool {
	_, ok := errors.AsType[wire.ResultError](err)
	return ok
}

// Dispatch names a RouteInfo's credential path; no route offers the Claude subscription (Anthropic terms forbid it).
const (
	RouteCopilotLogin  = "copilot-login"
	RouteOwnLogin      = "own-login"
	RouteByokAnthropic = "byok-anthropic"
	RouteDatabricks    = "databricks"
	RouteNone          = "none"
)

// RouteInfo is the dispatch `configure github`, `generate` and `doctor` report: credential path and why.
type RouteInfo struct {
	Agent, Route, Provider, Model, Reason string
	OK                                    bool
}

// DescribeRoute reports the fallback dispatch opencode|copilot resolves to; any other agent reports RouteNone.
func (s Schema) DescribeRoute(reg *providers.Registry) RouteInfo { return s.Eng().DescribeRoute(reg) }

// RouteTarget converts a resolved RouteInfo into a Target; ok is false for RouteNone or a provider the registry lost.
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

// RouteDetail is the agent's doctor "<agent> route" detail; ok is false for an unknown agent or a registry load error.
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

// ResolveTarget is sel's target when it serves the agent, else (explicit false) its own login or a keyed fallback.
func (s Schema) ResolveTarget(reg *providers.Registry, sel providers.Selection, explicit bool) (target providers.Target, note string, err error) {
	name, e := s.Name, s.Eng()
	t, ok := sel.Aliases[models.Opus]
	if !ok {
		return providers.Target{}, "", fmt.Errorf("%s: no opus target resolved", name)
	}
	// A CLI's own login is preferred ahead of a session provider that could serve it; an explicit --provider still wins.
	if pt, pnote, ok, perr := e.PreRoute(reg, explicit); ok {
		return pt, pnote, perr
	}
	if explicit || (CanServe(name, t.Provider) && e.FollowsPin(t.Provider)) {
		return e.Gate(reg, t)
	}
	if ft, fallbackNote, ok := e.FallbackTarget(reg); ok {
		return s.fallbackTarget(t, ft, fallbackNote)
	}
	// opencode and copilot have a defined fallback chain (DescribeRoute); nothing in it resolved either.
	if err := e.NoRoute(reg, t); err != nil {
		return providers.Target{}, "", err
	}
	return t, "", nil
}

// fallbackTarget is ResolveTarget on ft, the fallback for a session target t that cannot serve the agent.
func (s Schema) fallbackTarget(t, ft providers.Target, fallbackNote string) (providers.Target, string, error) {
	note := fmt.Sprintf("%s: session provider %s cannot serve %s; using %s", s.Name, t.Provider.Name, s.Name, fallbackNote)
	if own, ok := s.Eng().FallbackNote(ft, fallbackNote); ok {
		note = own
	}
	if ft.Provider == nil {
		return ft, note, ErrOwnLogin // the agent's own stored login: launch it plain, nothing to inject
	}
	return ft, note, nil
}

// ErrOwnLogin means the agent should run on the credentials its own CLI stores (no harness route injected).
var ErrOwnLogin = errors.New("agent runs on its own login")

// HubEngine is one hub ladder step: an Engine naming the provider account answering it, so Result.Provider shows which.
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

// isHubAgent reports whether ask routes name through opencode unless --native; codex and copilot run their own CLI.
func isHubAgent(name string) bool {
	s, ok := lookupRow(name)
	return ok && s.CLI.Hub
}

// hubLinked reports whether a linked vendor registered the opencode Hub.
func hubLinked() bool { return hub != nil }

// hubStart resolves the provider a hub agent's first dispatch targets.
func hubStart(reg *providers.Registry, agentName, explicitProvider string) (*providers.Provider, error) {
	if hub == nil {
		return nil, errNoHub
	}
	return hub.Start(reg, agentName, explicitProvider)
}

// newHubDispatch builds a hub dispatch Agent for agentName on provider; reg resolves a provider:model override.
func newHubDispatch(agentName string, provider *providers.Provider, reg *providers.Registry) Engine {
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

// firstOutputDeadline is the free-dispatch stall deadline from the registry, else OpencodeRouting's 90s floor.
func firstOutputDeadline() time.Duration {
	reg, err := providers.Load()
	if err != nil {
		return (providers.OpencodeRouting{}).FirstOutput()
	}
	return reg.Routing.Opencode.FirstOutput()
}

// ErrStalled is a dispatch silent past its stall deadline: fatal, so Do never retries but the chain falls through.
var ErrStalled = errors.New("dispatch stalled: no output within the stall deadline")

// watchFreeDispatch sets the idle timeout on r when billing is free; any other agent (or billing) passes through.
func watchFreeDispatch(r Engine, billing string) Engine {
	if billing != router.BillFree {
		return r
	}
	if hl, ok := r.(HubEngine); ok {
		return hl.WithFirstOutput(firstOutputDeadline())
	}
	return r
}

// Caps declares which RunOptions fields an agent honors; Prompt, Timeout and Attach are always honored.
type Caps struct {
	Model, Provider, Effort, WorkDir, SessionID, Tools, NoTools, Autonomous, Fleet, Memory, NoPersist, SafeMode, DebugFile, Direct Level
	ReadOnly                                                                                                                       Level
	SystemPrompt, AppendSystemPrompt, Env, OutputSchema, Persona                                                                   Level
	Permissions                                                                                                                    Level // reads RunOptions.Permissions
}

// Level is how an agent honors one flag: its own flag (Native), a harness shim (Emulated), or not at all (None).
type Level uint8

const (
	None Level = iota
	Emulated
	Native
)

func (l Level) String() string { return [...]string{"none", "emulated", "native"}[min(l, Native)] }

// Engine is one agent backend.
type Engine interface {
	Name() string
	Caps() Caps
	Available() error
	Run(ctx context.Context, req RunOptions, sink wire.Sink) (wire.Result, error)
}

// Engines lists every agent in roster order.
func Engines() []Engine {
	var out []Engine
	for _, s := range rows() {
		out = append(out, engineFor(s.Name))
	}
	return out
}

// engineFor is the registered roster agent name as an Engine: its own, else a CLI engine over its headless driver.
func engineFor(name string) Engine {
	if e, ok := mustDriver(name).(Engine); ok {
		return e
	}
	return newCLIEngine(name)
}

// Lookup finds an agent by name.
func Lookup(name string) (Engine, error) {
	if _, ok := lookupRow(name); ok {
		return engineFor(name), nil
	}
	return nil, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(RosterNames(), "|"))
}
