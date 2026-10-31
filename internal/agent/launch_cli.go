package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// LaunchPlan resolves `claude-code run <agent>`: the opus target via Configure, else the agent's own login.
func LaunchPlan(ctx context.Context, name string, o LaunchOptions, args []string) (Plan, error) {
	plan, err := cliPlan(ctx, name, o, args)
	plan.Keep, plan.CLI = o.KeepEnv, true
	if err == nil && !o.PrintEnv && !authArgs(args) && interactiveArgs(args) && agentVersionTTY() {
		if line := VersionWarning(name, plan.Binary); line != "" {
			fmt.Fprintln(versionWarnOut, line)
		}
	}
	return plan, err
}

// codexProfileHint is the usage error for `codex --profile X` when X is no harness profile but codex's own
// $CODEX_HOME/X.config.toml exists; nil otherwise.
func codexProfileHint(o LaunchOptions) error {
	if o.Provider == "" || strings.ContainsAny(o.Provider, `/\`) {
		return nil
	}
	home, err := CodexHome()
	if err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(home, o.Provider+".config.toml")); err != nil {
		return nil
	}
	return UsageError(fmt.Sprintf("claude-code codex: --profile %q is not a harness profile; "+
		"for codex's own config profile ($CODEX_HOME/%s.config.toml) use -p %s", o.Provider, o.Provider, o.Provider))
}

// agentVersionTTY and versionWarnOut are the version warning's terminal gate and sink (tests swap them).
var (
	agentVersionTTY           = func() bool { return ansi.IsTerminal(os.Stderr) }
	versionWarnOut  io.Writer = os.Stderr
)

// withFlagModel re-adds the --model ParseArgs consumed, so the registry selects flag > pin > default.
func withFlagModel(args []string, model string) []string {
	if model == "" {
		return args
	}
	return append([]string{"--model", model}, args...)
}

// agentModelTarget applies flag > the agent's model pin > default to t; a fallback provider respells the flag itself.
func agentModelTarget(name string, t providers.Target, sel providers.Selection, flagModel string) (providers.Target, string) {
	applied := t.Provider == nil || t.Provider == sel.Slots[models.Opus].Provider
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
	args, err := spec.eng().launchArgs(o.File, args)
	if err != nil {
		return Plan{}, err
	}
	flagModel := o.Model // the flag alone; the agent's model pin is applied after the target resolves
	o = withAgentPins(name, o)
	reg, sel, err := selection(o)
	if err != nil {
		if hint := spec.eng().profileHint(o); hint != nil {
			return Plan{}, hint
		}
		return Plan{}, err
	}
	// --profile or the agent's own profile pin is explicit: the session pin is Claude Code's default, not a per-agent choice.
	explicit := o.Provider != ""
	target, note, err := spec.ResolveTarget(reg, sel, explicit)
	if err == nil {
		var modelNote string
		if target, modelNote = agentModelTarget(name, target, sel, flagModel); modelNote != "" {
			note = strings.Join(slices.DeleteFunc([]string{note, modelNote}, func(s string) bool { return s == "" }), "; ")
		}
	}
	// An own-login note is only worth a line on an interactive launch (or --print-env): `opencode --version` stays quiet.
	showOwnLoginNote := o.PrintEnv || (ansi.IsTerminal(os.Stderr) && interactiveArgs(args))
	if errors.Is(err, ErrOwnLogin) {
		if showOwnLoginNote {
			fmt.Fprintln(NoticeOut, note)
		}
		return plainPlan(spec, args)
	}
	if err != nil {
		return Plan{}, err
	}
	if spec.eng().nativeRegistry(ctx, target) {
		return copilotRegistryPlan(ctx, spec, reg, target, note, withFlagModel(args, flagModel), o.PrintEnv)
	}
	env, prefix, err := spec.Configure(ctx, target, "")
	if errors.Is(err, ErrOwnLogin) {
		// The agent's own login (opencode on Copilot) picks the model itself: plain launch, not a failure.
		if showOwnLoginNote {
			fmt.Fprintln(NoticeOut, note)
		}
		return plainPlan(spec, args)
	}
	if note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	if err != nil {
		reason := fmt.Errorf("%w; %s", err, spec.CLI.ConfigureHint)
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
	}
	bin, err := lookPath(spec.CLI.Binary)
	if err != nil {
		return Plan{}, err
	}
	set := envMap(env)
	return Plan{Binary: bin, Set: set, Argv: append(prefix, args...)}, nil
}

// withAgentPins fills the profile the flags left empty from the agent's vault pin: --profile > pin > default.
// The model pin is applied after the target resolves (agentModelTarget).
func withAgentPins(name string, o LaunchOptions) LaunchOptions {
	pin, ok := AgentPins()[name]
	if !ok || pin.Off {
		return o
	}
	if o.Provider == "" && pin.Profile != "" {
		if reg, err := providers.Load(); err == nil && ValidAgentPin(reg, name, pin.Profile) == nil && reg.Providers[pin.Profile] != nil {
			o.Provider = pin.Profile
		}
	}
	return o
}

// copilotRegistryPlan is the interactive Databricks copilot launch on a native model registry. It fails closed:
// no error here reaches passthrough or the plain CLI, which would run on copilot's own GitHub login.
func copilotRegistryPlan(
	ctx context.Context, spec Schema, reg *providers.Registry, t providers.Target, note string, args []string,
	dry bool,
) (Plan, error) {
	if note != "" {
		fmt.Fprintln(NoticeOut, note)
	}
	cr, err := ConfigureCopilotRegistry(ctx, reg, t, args, dry)
	if err != nil {
		return Plan{}, err
	}
	bin, err := lookPath(spec.CLI.Binary)
	if err != nil {
		cr.Revoke()
		return Plan{}, err
	}
	lenv, lprefix := spec.LaunchSpec()
	fmt.Fprintln(NoticeOut, CopilotRegistryBanner)
	return Plan{
		Binary: bin, Set: envMap(append(lenv, cr.Env...)), Unset: cr.Unset, Warnings: cr.Warnings,
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

// codexFileOverrides flattens a config.toml (top-level keys and [a.b] tables, one-line values) into
// codex `-c key=value` pairs; values pass through as TOML, which codex parses itself.
func codexFileOverrides(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	defer f.Close()
	var out []string
	table := ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[["):
			return nil, fmt.Errorf("-f %s:%d: arrays of tables are not supported", path, n)
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			table = strings.TrimSpace(line[1:len(line)-1]) + "."
		default:
			k, v, ok := strings.Cut(line, "=")
			v = strings.TrimSpace(v)
			if !ok || v == "" || strings.HasSuffix(v, "[") || strings.HasSuffix(v, "{") || strings.HasPrefix(v, `"""`) {
				return nil, fmt.Errorf("-f %s:%d: want one-line `key = value`", path, n)
			}
			out = append(out, "-c", table+strings.TrimSpace(k)+"="+v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	return out, nil
}
