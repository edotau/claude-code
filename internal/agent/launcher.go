package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// IsTerminal is the TTY check, a seam tests swap.
var IsTerminal = proc.IsTerminal

// flagValue is the value of the flag at args[i]: val when inline (--flag=val), else the next arg and its index.
func flagValue(args []string, i int, name, val string, hasVal bool) (string, int, error) {
	if hasVal {
		return val, i, nil
	}
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("%s needs a value", name)
	}
	return args[i+1], i + 1, nil
}

// ParseArgs strips harness flags from args (the first "--" ends them) and refuses a --keep-env name ask would refuse.
func ParseArgs(args []string, claude bool) (LaunchOptions, []string, error) {
	var o LaunchOptions
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		case a == "--direct" && !claude:
			o.Direct = true
		case a == "--print-env":
			o.PrintEnv = true
		default:
			setter := o.launchFlagSetter(name, claude)
			if setter == nil {
				rest = append(rest, a)
				continue
			}
			var err error
			if val, i, err = flagValue(args, i, name, val, hasVal); err != nil {
				return o, nil, err
			}
			setter(val)
		}
	}
	if _, err := agentconfig.FlagOverlay(nil, o.KeepEnv); err != nil { // names only: never a value
		return LaunchOptions{}, nil, err
	}
	return o, rest, nil
}

// UsageError is a launch refusal the CLI reports verbatim with exit 2.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// SessionSelection loads the registry and applies --profile/--model > last profile > pins; session marks Claude.
func SessionSelection(o LaunchOptions, session bool) (*providers.Registry, providers.Selection, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Selection{}, err
	}
	if session && o.Provider == "" { // this workspace beats the global pin, which only seeds new workspaces
		o.Provider = lastProfile(reg, "claude")
	}
	sel, err := providers.Select(reg, o.Provider)
	if o.Model != "" && sel.Provider != nil {
		applyModel(&sel, o.Model, session, o.Alias)
		err = nil
	}
	return reg, sel, err
}

// lastProfile is the profile this workspace last launched agentName with, "" unless registered and usable by the agent.
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

// MainAlias is the alias whose model is the launch's main model: the one --model names, else the verb's, else opus.
func MainAlias(o LaunchOptions) string {
	if t, _, _ := parseModelFlag(o.Model); t != "" {
		return t
	}
	return cmp.Or(o.Alias, models.Opus)
}

// parseModelFlag reads --model: the Claude family alias, its catalog id, and whether it was a bare (floating) alias.
func parseModelFlag(s string) (alias, canonical string, bare bool) {
	s = strings.TrimSpace(s)
	name := strings.ToLower(models.Strip1M(s))
	if c, ok := models.Alias(name); ok && slices.Contains(models.AliasNames, name) {
		return name, c, !models.Has1M(s)
	}
	canonical, _, ok := models.Resolve(s)
	if !ok {
		return "", "", false
	}
	if f, _, _, ok := models.ParseClaudeID(canonical); ok && slices.Contains(models.AliasNames, f) {
		return f, canonical, false
	}
	return "", canonical, false
}

// applyModel applies --model to sel: a bare alias keeps the alias's resolution, else the main alias.
func applyModel(sel *providers.Selection, model string, session bool, verbAlias string) {
	alias, canonical, bare := parseModelFlag(model)
	dst := models.Opus
	if session {
		dst = MainAlias(LaunchOptions{Model: model, Alias: verbAlias})
	}
	if bare {
		if t, ok := sel.Aliases[alias]; ok && dst != alias {
			sel.Aliases[dst] = t
		}
		return
	}
	wire := strings.TrimSpace(model)
	oneM := models.Has1M(wire)
	wire = models.Strip1M(wire)
	if canonical != "" {
		wire = sel.Provider.WireModel(canonical)
	} else if sel.Provider != nil { // preflight's spelling: a bare id the workspace serves runs as databricks-<id>
		wire = launchWire("claude", sel.Provider, wire)
	}
	sel.Aliases[dst] = providers.Target{Provider: sel.Provider, Model: wire, OneM: oneM}
}

// IsHeadless reports a `claude -p`/`--print` run, the only launch that carries the setup-token.
func IsHeadless(args []string) bool {
	return slices.Contains(args, "-p") || slices.Contains(args, "--print")
}

// ParseLaunchArgs refuses a removed flag (with its hint) before ParseArgs strips the harness flags.
func ParseLaunchArgs(args []string, claude bool) (LaunchOptions, []string, error) {
	if err := helptext.RemovedFlagHint(args); err != nil {
		return LaunchOptions{}, nil, err
	}
	return ParseArgs(args, claude)
}

// RunPlan applies the launch overlay to plan, then prints it (--print-env) or execs it; a *UsageError exits 2.
func RunPlan(name string, plan Plan, opts LaunchOptions, err error) int {
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
		oerr = plan.checkOverlay(ov) // a launch-owned route name is refused, not silently overridden
	}
	if oerr != nil {
		if plan.OnExecFail != nil {
			plan.OnExecFail() // a minted session token dies with the refused launch
		}
		fmt.Fprintln(os.Stderr, "claude-code:", oerr)
		return 2
	}
	plan = plan.withOverlay(ov)
	if opts.PrintEnv {
		plan.Print(os.Stdout)
		if agentconfig.OverlayDebugOn() {
			plan.recordEnv() // dry exec: the env Exec would hand the child
		}
		return 0
	}
	return helptext.Fail("exec %s: %v", plan.Binary, plan.Exec())
}

// launchOverlay is --overlay, else $COWORKER_OVERLAY; --print-env keeps vault refs unresolved (names, never values).
func launchOverlay(opts LaunchOptions) (agentconfig.Resolved, error) {
	o, err := agentconfig.LoadOverlay(opts.Overlay, os.Stdin)
	if err != nil {
		return agentconfig.Resolved{}, err
	}
	if p, prof, _ := router.Coworker(); opts.Overlay == "" && p != "" && o.Provider == p && o.Profile == prof {
		o.Provider, o.Profile = "", "" // the inherited confinement, not this launch's routing (withOverlay hands it on)
	}
	if o.Routed() { // an interactive launch takes the env layer only: refuse, never silently ignore
		return agentconfig.Resolved{}, errors.New("overlay " + strings.Join(agentconfig.RoutingKeys(), "/") +
			" apply to agents ask and gateway, not an interactive launch")
	}
	if opts.PrintEnv {
		return agentconfig.Resolved{Set: o.Set, Unset: o.Unset, Keep: o.Keep}, nil
	}
	return agentconfig.ResolveOverlay(o)
}

// LaunchPlan resolves `claude-code <agent>`: the opus target via Configure, else the agent's own login.
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
	if err := refuseDirect(o.Direct); err != nil {
		return Plan{}, err
	}
	if o.Direct {
		ctx = WithDirect(ctx)
	}
	spec, ok := LookupSchema(name)
	if !ok || !spec.External() {
		return Plan{}, fmt.Errorf("unknown agent %q (%s)", name, strings.Join(DefaultAgents, "|"))
	}
	if authArgs(args) {
		return plainPlan(spec, args) // signing in needs no model route; resolving one first blocks the very login it needs
	}
	args, err := spec.Eng().LaunchArgs(o.File, args)
	if err != nil {
		return Plan{}, err
	}
	flagModel := o.Model // the flag alone; the agent's model pin is applied after the target resolves
	o, err = launchProfile(name, o)
	if err != nil {
		return Plan{}, err
	}
	reg, sel, err := SessionSelection(o, false)
	if err != nil {
		if hint := spec.Eng().ProfileHint(o); hint != nil {
			return Plan{}, hint
		}
		return Plan{}, err
	}
	return cliLaunch(ctx, spec, name, o, args, flagModel, reg, sel)
}

// cliLaunch resolves the target for a selected profile and builds the registry, configured or own-login plan.
func cliLaunch(ctx context.Context, spec Schema, name string, o LaunchOptions, args []string, flagModel string,
	reg *providers.Registry, sel providers.Selection) (Plan, error) {
	// --profile or the agent profile pin is explicit: the session pin is the default, not a per-agent choice.
	explicit := o.Provider != ""
	target, note, err := cliTarget(spec, name, reg, sel, explicit, flagModel)
	showOwnLoginNote := ownLoginNoteShown(o, args)
	if errors.Is(err, ErrOwnLogin) {
		return ownLoginPlan(spec, withFlagModel(args, flagModel), note, showOwnLoginNote)
	}
	if err != nil {
		return Plan{}, err
	}
	if spec.Eng().NativeRegistry(ctx, target) {
		plan, err := registryPlan(ctx, spec, reg, target, note, withFlagModel(args, flagModel), o.PrintEnv)
		plan.Profile = o.Provider
		return plan, err
	}
	env, prefix, err := spec.Configure(ctx, target)
	if errors.Is(err, ErrOwnLogin) {
		// The agent's own login (opencode on Copilot) picks the model itself: plain launch, not a failure.
		return ownLoginPlan(spec, withFlagModel(args, flagModel), note, showOwnLoginNote)
	}
	if note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	if err != nil {
		return configureFailed(ctx, spec, name, o, args, flagModel, err)
	}
	return finishPlan(spec, o, args, env, prefix)
}

// ownLoginNoteShown: an own-login note is only worth a line on an interactive launch or --print-env.
func ownLoginNoteShown(o LaunchOptions, args []string) bool {
	return o.PrintEnv || (IsTerminal(os.Stderr) && interactiveArgs(args)) // `opencode --version` stays quiet
}

// ownLoginPlan is the plain launch of an agent that signs in itself, printing note when show.
func ownLoginPlan(spec Schema, args []string, note string, show bool) (Plan, error) {
	if show && note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	return plainPlan(spec, args)
}

// configureFailed offers configure once to a human at a terminal, then retries or falls back to a passthrough launch.
func configureFailed(ctx context.Context, spec Schema, name string, o LaunchOptions, args []string, flagModel string, err error) (Plan, error) {
	reason := fmt.Errorf("%w; %s", err, spec.CLI.ConfigureHint)
	var env, prefix []string
	// A human at a terminal is offered configure once; a yes re-resolves on the new state.
	if OfferConfigure != nil && !o.PrintEnv && OfferConfigure(name, reason) {
		env, prefix, err = retryConfigure(ctx, spec, name, o, flagModel)
	}
	plainArgs := withFlagModel(args, flagModel) // the plain CLI takes --model itself; the routed plan does not
	if errors.Is(err, ErrOwnLogin) {
		return plainPlan(spec, plainArgs)
	}
	if err != nil {
		return passthrough(spec, name, reason, plainArgs)
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

// launchProfile resolves the profile: --profile (passed through untouched) > this workspace last-used > the agent pin.
func launchProfile(name string, o LaunchOptions) (LaunchOptions, error) {
	if o.Provider != "" {
		return o, nil
	}
	last := lastUsedProfile(name)
	cfg, err := agentconfig.For(name, agentconfig.Launch{LastProfile: last})
	if err != nil {
		return LaunchOptions{}, err
	}
	if o.Provider = cfg.Profile; cfg.Profile != last {
		o.Provider, err = pinRoute(name, cfg.Profile)
	}
	return o, err
}

// pinRoute is a profile pin as the launch routes it: "" for the agent own login or a non-registry pin id (opencode-go).
func pinRoute(name, pin string) (string, error) {
	if pin == "" {
		return "", nil
	}
	if own, err := OwnLogin(name, pin); own || err != nil {
		return "", err
	}
	reg, err := providers.Load()
	if err != nil || reg.Providers[pin] == nil {
		return "", err
	}
	return pin, nil
}

// lastUsedProfile is this workspace's last-used profile for an active agent CLI, "" when none applies.
func lastUsedProfile(name string) string {
	if !slices.Contains(agentid.External(), name) || !AgentActive(name) {
		return ""
	}
	if reg, err := providers.Load(); err == nil {
		return lastProfile(reg, name)
	}
	return ""
}

// registryPlan is the launch on a CLI native model registry (copilot on Databricks); it fails closed, no passthrough.
func registryPlan(
	ctx context.Context, spec Schema, reg *providers.Registry, t providers.Target, note string, args []string,
	dry bool,
) (Plan, error) {
	if note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	cr, err := spec.Eng().RegistryLaunch(ctx, reg, t, args, dry)
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

// retryConfigure re-resolves the target after configure, applying the agent's model pin as cliLaunch does.
func retryConfigure(ctx context.Context, spec Schema, name string, o LaunchOptions, flagModel string) ([]string, []string, error) {
	reg, sel, err := SessionSelection(o, false)
	if err != nil {
		return nil, nil, err
	}
	target, note, err := cliTarget(spec, name, reg, sel, o.Provider != "", flagModel)
	if err != nil {
		return nil, nil, err
	}
	if note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	return spec.Configure(ctx, target)
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

// OfferConfigure is the seam `configure` installs: asked once for an unroutable agent, true re-resolves; nil = none.
var OfferConfigure func(name string, reason error) bool

// passthrough launches the agent own CLI untouched when the harness cannot route it: its own sign-in stays reachable.
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

// envMap turns a KEY=VALUE env list into a map, dropping malformed entries.
func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// lookPath is the seam for resolving an agent binary.
var lookPath = paths.AgentBin
