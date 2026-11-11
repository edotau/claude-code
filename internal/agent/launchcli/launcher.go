// Package launchcli is the launch surface the vendor drivers share: harness flag parsing, profile selection and exec.
package launchcli

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
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
func ParseArgs(args []string, claude bool) (agent.LaunchOptions, []string, error) {
	var o agent.LaunchOptions
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
			setter := o.LaunchFlagSetter(name, claude)
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
		return agent.LaunchOptions{}, nil, err
	}
	return o, rest, nil
}

// UsageError is a launch refusal the CLI reports verbatim with exit 2.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// SessionSelection loads the registry and applies --profile/--model > the workspace's last profile > pins > defaults;
// session marks a Claude launch.
func SessionSelection(o agent.LaunchOptions, session bool) (*providers.Registry, providers.Selection, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, providers.Selection{}, err
	}
	if session && o.Provider == "" { // this workspace beats the global pin, which only seeds new workspaces
		o.Provider = LastProfile(reg, "claude")
	}
	sel, err := providers.Select(reg, o.Provider)
	if o.Model != "" && sel.Provider != nil {
		applyModel(&sel, o.Model, session, o.Alias)
		err = nil
	}
	return reg, sel, err
}

// LastProfile is the profile this workspace last launched agentName with, "" unless it is registered and the agent can use it.
func LastProfile(reg *providers.Registry, agentName string) string {
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

// MainAlias is the alias whose model is the launch's main model: the one --model names, else the verb's, else opus.
func MainAlias(o agent.LaunchOptions) string {
	if t, _, _ := parseModelFlag(o.Model); t != "" {
		return t
	}
	return cmp.Or(o.Alias, models.Opus)
}

// parseModelFlag reads --model: alias is the Claude family it names ("" for a non-Claude wire id), canonical its
// catalog id ("" when unknown), bare is true when the input was a bare alias (opus), which floats.
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
		dst = MainAlias(agent.LaunchOptions{Model: model, Alias: verbAlias})
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
	}
	sel.Aliases[dst] = providers.Target{Provider: sel.Provider, Model: wire, OneM: oneM}
}

// IsHeadless reports a `claude -p`/`--print` run, the only launch that carries the setup-token.
func IsHeadless(args []string) bool {
	return slices.Contains(args, "-p") || slices.Contains(args, "--print")
}

// ParseLaunchArgs refuses a removed flag (with its hint) before ParseArgs strips the harness flags.
func ParseLaunchArgs(args []string, claude bool) (agent.LaunchOptions, []string, error) {
	if err := helptext.RemovedFlagHint(args); err != nil {
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
