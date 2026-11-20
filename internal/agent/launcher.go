package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
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
	if err := o.foldOverlay(); err != nil {
		return o, rest, err
	}
	if err := castLaunchModel(&o); err != nil {
		return LaunchOptions{}, nil, &UsageError{Msg: err.Error()}
	}
	return o, rest, nil
}

// castLaunchModel reads a launch's --model <profile>:<id> as --profile <profile> --model <id>.
func castLaunchModel(o *LaunchOptions) error {
	c, err := providers.CastShared(o.Model)
	if err == nil {
		_, o.Provider, err = c.Merge("", o.Provider)
		o.Model = c.Model
	}
	return err
}

// UsageError is a launch refusal the CLI reports verbatim with exit 2.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// SessionSelection loads the registry; a Claude launch (session) routes --model through ResolveModel, else the pins.
func SessionSelection(o LaunchOptions, session bool) (*providers.Registry, providers.Selection, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Selection{}, err
	}
	if o.Model == "" || !session { // an external launch routed o.Model already (routeLaunch) and sets it on its target
		sel, err := providers.Select(reg, o.Provider)
		return reg, sel, err
	}
	r, err := launchRoute(context.Background(), reg, agentid.Claude, o)
	if err != nil {
		return nil, providers.Selection{}, err
	}
	sel, err := providers.Select(reg, r.Profile)
	if err != nil {
		return nil, providers.Selection{}, err
	}
	applyRoute(&sel, r, o, session)
	return reg, sel, nil
}

// launchRoute is ResolveRun for an interactive launch: o.Provider is the typed --profile, the agent pin the rest.
func launchRoute(ctx context.Context, reg *providers.Registry, name string, o LaunchOptions) (ModelRoute, error) {
	q := QueryFor(valuesContext(ctx, o.Values), ModelQuery{Agent: name, Model: o.Model, Profile: o.Provider, Interactive: !o.Dispatch})
	q.Session = "" // a launch has no session lead: the pin decides
	if err := refuseOwnAuto(q.Agent, q.Model); err != nil {
		return ModelRoute{}, err
	}
	resolve := ResolveRun
	if ownConnectorQuery(q) {
		resolve = ResolveModel // a launch checks an own-plan model against the row; only a run reports it unrouted
	}
	r, err := resolve(reg, q, time.Now())
	if errors.Is(err, ErrModelRoute) {
		return r, &UsageError{Msg: "--model " + o.Model + ": " + strings.TrimPrefix(err.Error(), ErrModelRoute.Error()+": ")}
	}
	return r, err
}

// applyRoute points sel's main alias at r: a bare alias keeps that alias's resolution, else the spelled model.
func applyRoute(sel *providers.Selection, r ModelRoute, o LaunchOptions, session bool) {
	dst := models.Opus
	if session {
		dst = MainAlias(o)
	}
	if t, ok := sel.Aliases[r.Alias]; ok && r.Alias != "" && strings.ToLower(modelPart(o.Model)) == r.Alias {
		sel.Aliases[dst] = t // bare alias, e.g. opus or ultra:opus
		return
	}
	sel.Aliases[dst] = providers.Target{Provider: sel.Provider, Model: r.Model, OneM: r.OneM}
}

// MainAlias is the alias whose model is the launch's main model: the one --model names, else the verb's, else opus.
func MainAlias(o LaunchOptions) string {
	if a := typedAlias(o.Model); a != "" {
		return a
	}
	return cmp.Or(o.Alias, models.Opus)
}

// modelPart is a --model without its "<profile>:" prefix.
func modelPart(s string) string {
	if _, rest, ok := strings.Cut(strings.TrimSpace(s), ":"); ok && rest != "" {
		return rest
	}
	return strings.TrimSpace(s)
}

// typedAlias is the Claude family alias a --model names (alias or catalog id, after any "<profile>:"), "" when none.
func typedAlias(s string) string {
	s = modelPart(s)
	if name := strings.ToLower(models.Strip1M(strings.TrimSpace(s))); slices.Contains(models.AliasNames, name) {
		return name
	}
	canonical, _, ok := models.Resolve(strings.TrimSpace(s))
	if !ok {
		return ""
	}
	if f, _, _, ok := models.ParseClaudeID(canonical); ok && slices.Contains(models.AliasNames, f) {
		return f
	}
	return ""
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
	if opts.Values == nil {
		opts.Values = plan.Values
	}
	if usage, ok := errors.AsType[*UsageError](err); ok {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if form := cloudForm(name, plan.Argv); form != "" {
		return helptext.Fail("%s sends the repo to the vendor's cloud: use %s", name, form)
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
	o, err := agentconfig.LoadOverlayContext(valuesContext(context.Background(), opts.Values), opts.Overlay, os.Stdin)
	if err != nil {
		return agentconfig.Resolved{}, err
	}
	if p, prof, _ := router.CoworkerContext(valuesContext(context.Background(), opts.Values)); opts.Overlay == "" && p != "" && o.Provider == p && o.Profile == prof {
		o.Provider, o.Profile = "", "" // the inherited confinement, not this launch's routing (withOverlay hands it on)
	}
	if o.BudgetUSD != nil || o.BudgetCredits != nil { // a budget needs the ask loop to meter it
		return agentconfig.Resolved{}, errors.New("overlay budgetUSD/budgetCredits apply to agents ask only")
	}
	if key := refusedLaunchKey(o, opts.Overlay != ""); key != "" { // refuse, never silently ignore
		return agentconfig.Resolved{}, errors.New("overlay " + key + " applies to agents ask and gateway, not an interactive launch")
	}
	if opts.PrintEnv {
		return agentconfig.Resolved{Set: o.Set, Unset: o.Unset, Keep: o.Keep}, nil
	}
	return agentconfig.ResolveOverlay(o)
}

// refusedLaunchKey names the first overlay key a launch cannot honor; the route, model and effort fold in ParseArgs.
func refusedLaunchKey(o agentconfig.Overlay, explicit bool) string {
	switch {
	case o.Fallback != "":
		return "fallback"
	case o.TokenCommand != nil:
		return "tokenCommand"
	case o.CodexNetwork:
		return "codexNetwork"
	case o.Cloud:
		return "cloud"
	case !explicit && (!o.Route().IsZero() || o.Model != "" || o.Effort != ""): // only --overlay routes a launch
		return "provider/profile/model/effort"
	}
	return ""
}

// foldOverlay inlines a --overlay once (stdin reads once) and lets its route and model fill what no flag typed.
func (o *LaunchOptions) foldOverlay() error {
	if o.Overlay == "" {
		return nil
	}
	ov, err := agentconfig.LoadOverlayContext(context.Background(), o.Overlay, os.Stdin)
	if err != nil {
		return err
	}
	doc, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	o.Overlay = string(doc)
	r := ov.Route()
	if o.Provider == "" && !r.IsZero() { // the route comes whole from one source: a typed --profile wins it all
		o.Route = r
		o.Provider = r.Profile
		if r.Profile == "auto" {
			o.Provider, o.Model = "", cmp.Or(o.Model, r.Provider+":"+cmp.Or(ov.Model, "auto"))
		}
	}
	o.Model = cmp.Or(o.Model, ov.Model)
	return nil
}

// LaunchPlan resolves `claude-code <agent>`: the opus target via Configure, else the agent's own login.
func LaunchPlan(ctx context.Context, name string, o LaunchOptions, args []string) (Plan, error) {
	ctx, o.Values = RunValues(ctx, o.Values)
	ctx = context.WithValue(ctx, launchDryKey{}, o.PrintEnv)
	c, _, err := providers.EnvCast(ctx, name)
	if err == nil && o.Model == "" { // the env stays the agent's pin; only its route unit joins
		_, o.Provider, err = c.Merge("", o.Provider)
	}
	if err != nil {
		return Plan{}, &UsageError{Msg: err.Error()}
	}
	plan, err := cliPlan(ctx, name, o, args)
	plan.Values = o.Values.Merge(nil)
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

func cliPlan(ctx context.Context, name string, o LaunchOptions, args []string) (Plan, error) {
	if err := refuseDirectContext(ctx, o.Direct); err != nil {
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
	if slices.Contains(args, "--read-only") { // Wire sees no args: the flag rides the context
		ctx = WithReadOnly(ctx)
	}
	args, err := spec.Eng().LaunchArgs(o.File, args)
	if err != nil {
		return Plan{}, err
	}
	o, own, err := routeTyped(ctx, name, o)
	if err != nil {
		return Plan{}, err
	}
	flagModel := o.Model // the spelled flag alone; the agent's model pin is applied after the target resolves
	if own {
		return ownLoginPlan(spec, withFlagModel(args, flagModel), "", ownLoginNoteShown(o, args))
	}
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

// routeTyped resolves the typed --model once, else (an agent with no login of its own, unpinned) the pool's pick.
func routeTyped(ctx context.Context, name string, o LaunchOptions) (_ LaunchOptions, own bool, err error) {
	o, pooled, err := poolLaunch(name, o)
	if err != nil || o.Model == "" { // the typed model resolves once; every launch below spells from that route
		return o, false, err
	}
	o, own, err = routeLaunch(ctx, name, o)
	if pooled {
		err = poolPicked(name, o.Provider, err)
	}
	return o, own, err
}

// poolLaunch sends an unpinned, untyped launch of an agent with no login of its own to the Databricks pool pick.
func poolLaunch(name string, o LaunchOptions) (LaunchOptions, bool, error) {
	if !agentid.Routing(name).NoOwnLogin || o.Provider != "" || o.Model != "" {
		return o, false, nil
	}
	pinned, err := launchProfile(name, o)
	if err != nil || pinned.Provider != "" {
		return o, false, err
	}
	o.Provider, o.Model = poolName, AutoModel // the pool's own route, as the overlay {"provider":"databricks","profile":"auto"}
	return o, true, nil
}

// poolPicked is the stderr line naming the pool's pick, or the refusal that names the two ways forward.
func poolPicked(name, profile string, err error) error {
	if err != nil {
		return &UsageError{Msg: fmt.Sprintf("%s: no usable Databricks profile in the pool (%v); run `claude-code %s --profile <dev|qa|prod>` "+
			"or `claude-code databricks login`", name, strings.TrimPrefix(err.Error(), "--model "+AutoModel+": "), name)}
	}
	fmt.Fprintf(NoticeOut, "claude-code: %s: no profile pinned; picked %s from the Databricks pool\n", name, profile)
	return nil
}

// routeLaunch resolves o.Model into the row and wire id the launch runs; own is true on the agent's own sign-in.
func routeLaunch(ctx context.Context, name string, o LaunchOptions) (_ LaunchOptions, own bool, _ error) {
	reg, err := providers.Load()
	if err != nil {
		return o, false, err
	}
	r, err := launchRoute(ctx, reg, name, o)
	if err != nil {
		return o, false, err
	}
	o.Model, o.Provider = r.Model, r.Profile
	if r.Effort != "" && agentid.Routing(name).EffortSuffix {
		o.Model += "-" + r.Effort // gemini reads its effort off the id's suffix
	}
	if r.Dialect == "own-login" {
		o.Provider = ""
	}
	return o, r.Dialect == "own-login", nil
}

// cliLaunch resolves the target for a selected profile and builds the registry, configured or own-login plan.
func cliLaunch(ctx context.Context, spec Schema, name string, o LaunchOptions, args []string, flagModel string,
	reg *providers.Registry, sel providers.Selection) (Plan, error) {
	// --profile or the agent profile pin is explicit: the session pin is the default, not a per-agent choice.
	explicit := o.Provider != ""
	target, note, err := cliTarget(ctx, spec, name, reg, sel, explicit, flagModel)
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
	plan, err := plainPlan(spec, args)
	if err != nil {
		return Plan{}, err
	}
	if show && note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	return plan, nil
}

// configureFailed offers configure once to a human at a terminal, then retries or falls back to a passthrough launch.
func configureFailed(ctx context.Context, spec Schema, name string, o LaunchOptions, args []string, flagModel string, err error) (Plan, error) {
	reason := err
	if hint := spec.CLI.ConfigureHint; hint != "" && !strings.Contains(err.Error(), hint) {
		reason = fmt.Errorf("%w; %s", err, hint)
	}
	var env, prefix []string
	// A human at a terminal is offered configure once; a yes re-resolves on the new state.
	if OfferConfigure != nil && !o.PrintEnv && OfferConfigure(name, reason) {
		env, prefix, err = retryConfigure(ctx, spec, name, o, flagModel)
	}
	plainArgs := withFlagModel(args, flagModel) // the plain CLI takes --model itself; the routed plan does not
	if errors.Is(err, ErrOwnLogin) {
		return plainPlan(spec, plainArgs)
	}
	if usage, ok := errors.AsType[*UsageError](err); ok {
		return Plan{}, usage // a typed refusal exits 2, never a "sign in" passthrough
	}
	if err != nil {
		if o.Provider != "" {
			return Plan{}, reason
		}
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
	return Plan{Binary: bin, Set: proc.ValuesFromEnv(env), Argv: append(prefix, args...), Profile: o.Provider}, nil
}

// cliTarget resolves spec's target, then sets the routed model, else the agent's pin, on it.
func cliTarget(ctx context.Context, spec Schema, name string, reg *providers.Registry, sel providers.Selection, explicit bool, flagModel string) (providers.Target, string, error) {
	target, note, err := spec.ResolveTarget(ctx, RunOptions{}, reg, sel, explicit)
	if err != nil {
		return target, note, err
	}
	if flagModel != "" {
		target.Model = flagModel
		return target, note, nil
	}
	if name == "gemini" {
		return PinnedTarget(ctx, name, target), note, nil
	}
	target, modelNote, err := spec.routedModel(ctx, RunOptions{}, target, false)
	return target, joinNotes(note, modelNote), err
}

// launchProfile resolves the profile: --profile (passed through untouched), else the agent pin.
func launchProfile(name string, o LaunchOptions) (LaunchOptions, error) {
	if o.Provider != "" {
		return o, nil
	}
	recs, err := vault.ReadAgentRecords([]string{name})
	if err != nil {
		return LaunchOptions{}, err
	}
	o.Provider, err = pinRoute(name, pinProfile(recs[name]))
	return o, err
}

// pinRoute preserves registry profiles, including own logins; non-registry pins launch on the native CLI.
func pinRoute(name string, pin string) (string, error) {
	if pin == "" {
		return "", nil
	}
	reg, err := providers.Load()
	if err != nil {
		return "", err
	}
	if err := ValidAgentPin(reg, name, pin); err != nil {
		return "", err
	}
	if reg.Providers[pin] == nil {
		return "", nil
	}
	return pin, nil
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
		Binary: bin, Set: proc.ValuesFromEnv(append(lenv, cr.Env...)), Unset: cr.Unset, Warnings: cr.Warnings,
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
	target, note, err := cliTarget(ctx, spec, name, reg, sel, o.Provider != "", flagModel)
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
	plan, err := plainPlan(spec, args)
	if err != nil {
		if usage, ok := errors.AsType[*UsageError](err); ok {
			return Plan{}, usage
		}
		return Plan{}, reason // no CLI to fall back to: the routing error is the whole story
	}
	fmt.Fprintf(NoticeOut, "%v\nlaunching the plain %s CLI on its own login (not routed by claude-code)\n", reason, name)
	return plan, nil
}

// plainPlan is the agent's own CLI on its own login: no routing env, but the agent spec's launch env/args still apply.
func plainPlan(spec Schema, args []string) (Plan, error) {
	if agentid.Routing(spec.Name).NoOwnLogin { // fail closed: such an agent's plain CLI would run with no harness guard
		return Plan{}, &UsageError{Msg: spec.Name + " has no login of its own; plain passthrough and --direct are refused: " +
			"run `claude-code " + spec.Name + " --profile <dev|qa|prod>`"}
	}
	opts, _, err := ParseArgs(args, false)
	if err != nil {
		return Plan{}, err
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(opts.Model)), models.DatabricksPrefix) {
		return Plan{}, &UsageError{Msg: spec.Name + ": a databricks- model needs a Databricks --profile; refused on own login"}
	}
	bin, err := lookPath(spec.CLI.Binary)
	if err != nil {
		return Plan{}, err
	}
	var env, prefix []string
	if spec.Name == "copilot" {
		st := spec.launchSettings(providers.Target{Model: opts.Model}, false)
		var notes []string
		var cleanup func()
		env, prefix, notes, cleanup, err = spec.Eng().RenderSettings(context.Background(), st, nil, nil)
		defer cleanup()
		if err != nil {
			return Plan{}, err
		}
		for _, note := range notes {
			fmt.Fprintln(NoticeOut, note)
		}
	} else {
		env, prefix = spec.LaunchSpec()
	}
	return Plan{Binary: bin, Set: proc.ValuesFromEnv(env), Argv: append(prefix, args...)}, nil
}

// lookPath is the seam for resolving an agent binary.
var lookPath = paths.AgentBin
