package launchplan

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

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

// selection loads the registry and applies the per-launch profile/model flags.
func selection(o agent.LaunchOptions) (*providers.Registry, providers.Selection, error) {
	return SessionSelection(o, false)
}

// SessionSelection loads the registry and applies --profile/--model > pins > the workspace's last profile > defaults;
// session marks a Claude launch.
func SessionSelection(o agent.LaunchOptions, session bool) (*providers.Registry, providers.Selection, error) {
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
