package launchplan

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// IsTerminal is the TTY check internal/cli wires at startup; the zero value says no terminal.
var IsTerminal = func(*os.File) bool { return false }

// LaunchPlan resolves `claude-code run <agent>`: the opus target via Configure, else the agent's own login.
func LaunchPlan(ctx context.Context, name string, o agent.LaunchOptions, args []string) (agent.Plan, error) {
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
	return agent.ResolveAgentModel(agent.ModelAsk{Agent: name, Flag: flagModel, Target: t, Applied: applied})
}

func cliPlan(ctx context.Context, name string, o agent.LaunchOptions, args []string) (agent.Plan, error) {
	if o.Direct {
		ctx = agent.WithDirect(ctx)
	}
	spec, ok := agent.LookupSchema(name)
	if !ok || !spec.External() {
		return agent.Plan{}, fmt.Errorf("unknown agent %q (codex|gemini|opencode|copilot)", name)
	}
	if authArgs(args) {
		return plainPlan(spec, args) // signing in needs no model route; resolving one first blocks the very login it needs
	}
	args, err := spec.Eng().LaunchArgs(o.File, args)
	if err != nil {
		return agent.Plan{}, err
	}
	flagModel := o.Model // the flag alone; the agent's model pin is applied after the target resolves
	o, err = launchProfile(name, o)
	if err != nil {
		return agent.Plan{}, err
	}
	reg, sel, err := SessionSelection(o, false)
	if err != nil {
		if hint := spec.Eng().ProfileHint(o); hint != nil {
			return agent.Plan{}, hint
		}
		return agent.Plan{}, err
	}
	return cliLaunch(ctx, spec, name, o, args, flagModel, reg, sel)
}

// cliLaunch resolves the target for a selected profile and builds the registry, configured or own-login plan.
func cliLaunch(ctx context.Context, spec agent.Schema, name string, o agent.LaunchOptions, args []string, flagModel string,
	reg *providers.Registry, sel providers.Selection) (agent.Plan, error) {
	// --profile or the agent's own profile pin is explicit: the session pin is Claude Code's default, not a per-agent choice.
	explicit := o.Provider != ""
	target, note, err := cliTarget(spec, name, reg, sel, explicit, flagModel)
	// An own-login note is only worth a line on an interactive launch (or --print-env): `opencode --version` stays quiet.
	showOwnLoginNote := o.PrintEnv || (IsTerminal(os.Stderr) && interactiveArgs(args))
	if errors.Is(err, agent.ErrOwnLogin) {
		return ownLoginPlan(spec, withFlagModel(args, flagModel), note, showOwnLoginNote)
	}
	if err != nil {
		return agent.Plan{}, err
	}
	if spec.Eng().NativeRegistry(ctx, target) {
		plan, err := registryPlan(ctx, spec, reg, target, note, withFlagModel(args, flagModel), o.PrintEnv)
		plan.Profile = o.Provider
		return plan, err
	}
	env, prefix, err := spec.Configure(ctx, target, "")
	if errors.Is(err, agent.ErrOwnLogin) {
		// The agent's own login (opencode on Copilot) picks the model itself: plain launch, not a failure.
		return ownLoginPlan(spec, withFlagModel(args, flagModel), note, showOwnLoginNote)
	}
	if note != "" {
		fmt.Fprintln(agent.NoticeOut, note)
	}
	if err != nil {
		return configureFailed(ctx, spec, name, o, args, flagModel, err)
	}
	return finishPlan(spec, o, args, env, prefix)
}

// ownLoginPlan is the plain launch of an agent that signs in itself, printing note when show.
func ownLoginPlan(spec agent.Schema, args []string, note string, show bool) (agent.Plan, error) {
	if show {
		fmt.Fprintln(agent.NoticeOut, note)
	}
	return plainPlan(spec, args)
}

// configureFailed offers configure once to a human at a terminal, then retries or falls back to a passthrough launch.
func configureFailed(ctx context.Context, spec agent.Schema, name string, o agent.LaunchOptions, args []string, flagModel string, err error) (agent.Plan, error) {
	reason := fmt.Errorf("%w; %s", err, spec.CLI.ConfigureHint)
	var env, prefix []string
	// A human at a terminal is offered configure once; a yes re-resolves on the new state.
	if OfferConfigure != nil && !o.PrintEnv && OfferConfigure(name, reason) {
		env, prefix, err = retryConfigure(ctx, spec, name, o, flagModel)
	}
	plainArgs := withFlagModel(args, flagModel) // the plain CLI takes --model itself; the routed plan does not
	if errors.Is(err, agent.ErrOwnLogin) {
		return plainPlan(spec, plainArgs)
	}
	if err != nil {
		return passthrough(spec, name, reason, plainArgs)
	}
	return finishPlan(spec, o, args, env, prefix)
}

// finishPlan locates the agent binary and assembles the configured launch.
func finishPlan(spec agent.Schema, o agent.LaunchOptions, args, env, prefix []string) (agent.Plan, error) {
	bin, err := lookPath(spec.CLI.Binary)
	if err != nil {
		return agent.Plan{}, err
	}
	return agent.Plan{Binary: bin, Set: envMap(env), Argv: append(prefix, args...), Profile: o.Provider}, nil
}

// cliTarget resolves spec's target, then applies the model flag and pin to it.
func cliTarget(spec agent.Schema, name string, reg *providers.Registry, sel providers.Selection, explicit bool, flagModel string) (providers.Target, string, error) {
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
// A --profile flag passes through untouched: selection validates it and the CLI's ProfileHint explains a miss.
func launchProfile(name string, o agent.LaunchOptions) (agent.LaunchOptions, error) {
	if o.Provider != "" {
		return o, nil
	}
	last := lastUsedProfile(name)
	cfg, err := agentconfig.For(name, agentconfig.Launch{LastProfile: last})
	if err != nil {
		return agent.LaunchOptions{}, err
	}
	if o.Provider = cfg.Profile; cfg.Profile != last {
		o.Provider, err = pinRoute(name, cfg.Profile)
	}
	return o, err
}

// pinRoute is a profile pin as the launch routes it: "" for the agent's own login or a non-registry pin id (opencode-go).
func pinRoute(name, pin string) (string, error) {
	if pin == "" {
		return "", nil
	}
	if own, err := agent.OwnLogin(name, pin); own || err != nil {
		return "", err
	}
	reg, err := providers.Load()
	if err != nil || reg.Providers[pin] == nil {
		return "", err
	}
	return pin, nil
}

// lastProfile is the profile this workspace last launched agentName with, "" unless it is registered and the agent can use it.
func lastProfile(reg *providers.Registry, agentName string) string {
	name := providers.LastProfile(agentName)
	p, err := reg.Get(name)
	if name == "" || err != nil {
		return ""
	}
	if agentName == "claude" && p.Auth.Login != "" || agentName != "claude" && !agent.CanServe(agentName, p) {
		return ""
	}
	return name
}

// lastUsedProfile is this workspace's last-used profile for an active agent CLI, "" when none applies.
func lastUsedProfile(name string) string {
	if (name != "copilot" && name != "opencode" && name != "codex" && name != "gemini") || !agent.AgentActive(name) {
		return ""
	}
	if reg, err := providers.Load(); err == nil {
		return lastProfile(reg, name)
	}
	return ""
}

// registryPlan is the interactive launch on a CLI's native model registry (copilot on Databricks). It fails closed:
// no error here reaches passthrough or the plain CLI, which would run on the CLI's own GitHub login.
func registryPlan(
	ctx context.Context, spec agent.Schema, reg *providers.Registry, t providers.Target, note string, args []string,
	dry bool,
) (agent.Plan, error) {
	if note != "" {
		fmt.Fprintln(agent.NoticeOut, note)
	}
	cr, err := spec.Eng().RegistryLaunch(ctx, reg, t, args, dry)
	if err != nil {
		return agent.Plan{}, err
	}
	bin, err := lookPath(spec.CLI.Binary)
	if err != nil {
		cr.Revoke()
		return agent.Plan{}, err
	}
	lenv, lprefix := spec.LaunchSpec()
	fmt.Fprintln(agent.NoticeOut, cr.Banner)
	return agent.Plan{
		Binary: bin, Set: envMap(append(lenv, cr.Env...)), Unset: cr.Unset, Warnings: cr.Warnings,
		Files:      cr.Files,
		Argv:       append(append(lprefix, cr.Argv...), cr.Args...),
		OnExecFail: cr.Revoke,
	}, nil
}

// retryConfigure re-resolves the target after configure, applying the agent's model pin as cliLaunch does.
func retryConfigure(ctx context.Context, spec agent.Schema, name string, o agent.LaunchOptions, flagModel string) ([]string, []string, error) {
	reg, sel, err := SessionSelection(o, false)
	if err != nil {
		return nil, nil, err
	}
	target, note, err := cliTarget(spec, name, reg, sel, o.Provider != "", flagModel)
	if err != nil {
		return nil, nil, err
	}
	if note != "" {
		fmt.Fprintln(agent.NoticeOut, note)
	}
	return spec.Configure(ctx, target, "")
}

// authArgs reports an agent's own sign-in subcommand: codex/copilot login|logout, gemini/opencode auth.
func authArgs(args []string) bool {
	return len(args) > 0 && (args[0] == "login" || args[0] == "logout" || args[0] == "auth")
}

// WantsVersionWarning reports whether a launch of args is an interactive session, the only one that warns on a version.
func WantsVersionWarning(o agent.LaunchOptions, args []string) bool {
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
func passthrough(spec agent.Schema, name string, reason error, args []string) (agent.Plan, error) {
	if _, err := lookPath(spec.CLI.Binary); err != nil {
		return agent.Plan{}, reason // no CLI to fall back to: the routing error is the whole story
	}
	fmt.Fprintf(agent.NoticeOut, "%v\nlaunching the plain %s CLI on its own login (not routed by claude-code)\n", reason, name)
	return plainPlan(spec, args)
}

// plainPlan is the agent's own CLI on its own login: no routing env, but the agent spec's launch env/args still apply.
func plainPlan(spec agent.Schema, args []string) (agent.Plan, error) {
	bin, err := lookPath(spec.CLI.Binary)
	if err != nil {
		return agent.Plan{}, err
	}
	env, prefix := spec.LaunchSpec()
	return agent.Plan{Binary: bin, Set: envMap(env), Argv: append(prefix, args...)}, nil
}

// Credentials ride these env vars (never argv); the config text names the variable, not the secret.
const (
	OpenCodeKeyEnv = "HARNESS_OPENCODE_API_KEY"
)

// Positional guards a dash-led prompt from flag parsing.
func Positional(prompt string) []string {
	if strings.HasPrefix(prompt, "-") {
		return []string{"--", prompt}
	}
	return []string{prompt}
}

// HeadlessLaunch applies name's spec Launch.Env (never Launch.Args) to argv, with the route env going last.
func HeadlessLaunch(name string, env, argv []string) (launchedEnv, launchedArgv []string) {
	launchEnv, argv := agent.SchemaFor(name).ApplyLaunch(nil, argv, false)
	return agent.RouteEnvLast(launchEnv, env), argv
}

// ApplyOverlay layers r under req's --env (the shorthand wins) and unsets $COWORKER_OVERLAY in the child (one hop).
func ApplyOverlay(req agent.RunOptions, r agentconfig.Resolved) agent.RunOptions {
	if agent.OverlayIdle(r) {
		return req
	}
	env := make([]string, 0, len(r.Set)+len(req.Env))
	for _, k := range slices.Sorted(maps.Keys(r.Set)) {
		env = append(env, k+"="+r.Set[k])
	}
	req.Env = append(env, req.Env...)
	req.KeepEnv = slices.Concat(req.KeepEnv, r.Keep)
	req.Unset = slices.Concat(req.Unset, r.Unset, []string{agentconfig.OverlayVar})
	req.OverlaySet = slices.Sorted(maps.Keys(r.Set))
	return req
}

// EnableOverlay checks path and points this process and its --job relaunches at it (absolute).
func EnableOverlay(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := agentconfig.LoadOverlay(abs); err != nil {
		return err
	}
	return os.Setenv(agentconfig.OverlayVar, abs)
}

// ParseLaunchArgs refuses a removed flag (with its hint) before ParseArgs strips the harness flags.
func ParseLaunchArgs(args []string, claude bool) (agent.LaunchOptions, []string, error) {
	if err := databricks.RemovedFlagHint(args); err != nil {
		return agent.LaunchOptions{}, nil, err
	}
	return ParseArgs(args, claude)
}

// Run applies the launch overlay to plan, then prints it (--print-env) or execs it; a *UsageError exits 2.
func Run(name string, plan agent.Plan, opts agent.LaunchOptions, err error) int {
	plan.Agent = name
	if usage, ok := errors.AsType[*UsageError](err); ok {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if err != nil {
		return helptext.Fail("%v", err)
	}
	ov, oerr := launchOverlay(opts)
	if oerr == nil {
		oerr = plan.CheckOverlay(ov) // a launch-owned route name is refused, not silently overridden
	}
	if oerr != nil {
		if plan.OnExecFail != nil {
			plan.OnExecFail() // a minted session token dies with the refused launch
		}
		fmt.Fprintln(os.Stderr, "claude-code:", oerr)
		return 2
	}
	plan = plan.WithOverlay(ov)
	if opts.PrintEnv {
		plan.Print(os.Stdout)
		if agentconfig.OverlayDebugOn() {
			plan.RecordEnv() // dry exec: the env Exec would hand the child
		}
		return 0
	}
	return helptext.Fail("exec %s: %v", plan.Binary, plan.Exec())
}

// launchOverlay is --overlay, else $COWORKER_OVERLAY; --print-env keeps vault refs unresolved (names, never values).
func launchOverlay(opts agent.LaunchOptions) (agentconfig.Resolved, error) {
	o, err := agentconfig.LoadOverlay(opts.Overlay)
	if err != nil {
		return agentconfig.Resolved{}, err
	}
	if opts.PrintEnv {
		return agentconfig.Resolved{Set: o.Set, Unset: o.Unset, Keep: o.Keep}, nil
	}
	return agentconfig.ResolveOverlay(o)
}
