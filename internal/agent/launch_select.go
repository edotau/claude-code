package agent

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// lookPath is the seam for resolving an agent binary.
var lookPath = paths.AgentBin

// selection loads the registry and applies the per-launch profile/model flags.
func selection(o LaunchOptions) (*providers.Registry, providers.Selection, error) {
	return SessionSelection(o, false)
}

// SessionSelection loads the registry and applies --profile/--model > pins > the workspace's last profile > defaults;
// session marks a Claude launch.
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

// MainAlias is the alias whose model is the launch's main model: the one --model names, else the verb's, else opus.
func MainAlias(o LaunchOptions) string {
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
	}
	sel.Aliases[dst] = providers.Target{Provider: sel.Provider, Model: wire, OneM: oneM}
}

// IsHeadless reports a `claude -p`/`--print` run, the only launch that carries the setup-token.
func IsHeadless(args []string) bool {
	return slices.Contains(args, "-p") || slices.Contains(args, "--print")
}

// SelfBinary is the resolved absolute path of the running harness binary.
func SelfBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Abs(exe)
}
