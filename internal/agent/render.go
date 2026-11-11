package agent

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// DefaultAgents is every agent generate renders when agentconfig.RenderOptions.Agents is empty.
var DefaultAgents = []string{agentid.Codex, agentid.OpenCode, agentid.Gemini, agentid.Copilot}

// RenderConfigs renders every selected agent's config and reports whether anything
// differs from what is on disk (Check) or would be written (DryRun never writes either way).
func RenderConfigs(o agentconfig.RenderOptions) (bool, error) {
	p, drift, err := PrepareConfigs(o)
	if err != nil {
		return drift, err
	}
	return drift, p.Apply()
}

// PrepareConfigs validates all selected renders without writing; dry-run/check output remains immediate.
func PrepareConfigs(o agentconfig.RenderOptions) (*agentconfig.RenderPlan, bool, error) {
	p := &agentconfig.RenderPlan{}
	o = o.Stage(p)
	var drift bool
	var err error
	agents := o.Agents
	if len(agents) == 0 {
		agents = DefaultAgents
	}
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	home := o.Home
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, false, fmt.Errorf("generate: %w", err)
		}
	}
	if skipConfigHomes(o) {
		fmt.Fprintf(out, "generate: skipping agent configs (CLAUDE_CONFIG_DIR=%s is neither ~/.claude nor an install clone); pass --agents to include them\n", paths.ConfigDir())
		return p, false, nil
	}
	reg, err := providers.Load()
	if err != nil {
		return nil, false, fmt.Errorf("generate: %w", err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		return nil, false, fmt.Errorf("generate: %w", err)
	}
	drift, err = prepareSelectedConfigs(agents, reg, sel, home, o, out)
	if err != nil {
		return nil, drift, err
	}
	return p, drift, nil
}

func skipConfigHomes(o agentconfig.RenderOptions) bool {
	return !o.ForceHomes && !paths.ConfigDirIsDefault() && !paths.ConfigDirIsInstall()
}

func prepareSelectedConfigs(agents []string, reg *providers.Registry, sel providers.Selection, home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	if err := knownAgents(agents); err != nil {
		return false, err
	}
	var drift bool
	for _, name := range agents {
		d, err := SchemaFor(name).Eng().Render(reg, sel, home, o, out)
		if err != nil {
			return drift, err
		}
		drift = drift || d
	}
	return drift, nil
}

// knownAgents rejects a name outside DefaultAgents, which render and revert would otherwise skip or no-op silently.
func knownAgents(agents []string) error {
	for _, a := range agents {
		if !slices.Contains(DefaultAgents, a) {
			return fmt.Errorf("generate: unknown agent %q (%s)", a, strings.Join(DefaultAgents, "|"))
		}
	}
	return nil
}

// RevertConfigs restores each selected agent's files to their newest backup version, removing the restored version so a second revert steps one version further back.
func RevertConfigs(o agentconfig.RenderOptions) error {
	agents := o.Agents
	if len(agents) == 0 {
		agents = DefaultAgents
	}
	if err := knownAgents(agents); err != nil {
		return err
	}
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	for _, a := range agents {
		files, err := SchemaFor(a).Eng().ConfigPaths(o.Home)
		if err != nil {
			return fmt.Errorf("generate: %w", err)
		}
		for _, lp := range files {
			if err := agentconfig.RevertOne(lp.Label, lp.Path, o.DryRun, out); err != nil {
				return err
			}
		}
	}
	return nil
}
