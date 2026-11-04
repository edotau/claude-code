package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
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
		return ownLoginPlan(spec, args, note, showOwnLoginNote)
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
		return ownLoginPlan(spec, args, note, showOwnLoginNote)
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

// withAgentPins fills the profile the flags left empty from the agent's vault pin: --profile > pin > default.
// The model pin is applied after the target resolves (agentModelTarget).
func withAgentPins(name string, o LaunchOptions) (LaunchOptions, error) {
	pins, err := AgentPins()
	if err != nil {
		return LaunchOptions{}, err
	}
	pin, ok := pins[name]
	if !ok || pin.Off {
		return o, nil
	}
	if o.Provider == "" && pin.Profile != "" {
		if reg, err := providers.Load(); err == nil && ValidAgentPin(reg, name, pin.Profile) == nil && reg.Providers[pin.Profile] != nil {
			o.Provider = pin.Profile
		}
	}
	return o, nil
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
	spec := schemaFor(name)
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
