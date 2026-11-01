// render.go renders agent configs and hooks without launching a session.
package agent

import (
	"fmt"
	"os"

	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// DefaultAgents is every agent generate renders when agentrender.RenderOptions.Agents is empty.
var DefaultAgents = []string{"codex", "opencode", "gemini", "copilot"}

// RenderConfigs renders every selected agent's config and reports whether anything
// differs from what is on disk (Check) or would be written (DryRun never writes either way).
func RenderConfigs(o agentrender.RenderOptions) (bool, error) {
	p, drift, err := PrepareConfigs(o)
	if err != nil {
		return drift, err
	}
	return drift, p.Apply()
}

// PrepareConfigs validates all selected renders without writing; dry-run/check output remains immediate.
func PrepareConfigs(o agentrender.RenderOptions) (*agentrender.RenderPlan, bool, error) {
	p := &agentrender.RenderPlan{}
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
	if !o.ForceHomes && !paths.ConfigDirIsDefault() {
		fmt.Fprintf(out, "generate: skipping agent configs (CLAUDE_CONFIG_DIR=%s is not the default ~/.claude); pass --agents to include them\n", paths.ConfigDir())
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
	want := map[string]bool{}
	for _, a := range agents {
		want[a] = true
	}
	for _, name := range DefaultAgents {
		if !want[name] {
			continue
		}
		d, err := schemaFor(name).eng().Render(reg, sel, home, o, out)
		if err != nil {
			return nil, drift, err
		}
		drift = drift || d
	}
	return p, drift, nil
}

// RevertConfigs restores each selected agent's files to their newest backup version, removing the restored version so a second revert steps one version further back.
func RevertConfigs(o agentrender.RenderOptions) error {
	agents := o.Agents
	if len(agents) == 0 {
		agents = DefaultAgents
	}
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	for _, a := range agents {
		files, err := schemaFor(a).eng().ConfigPaths(o.Home)
		if err != nil {
			return fmt.Errorf("generate: %w", err)
		}
		for _, lp := range files {
			if err := agentrender.RevertOne(lp.Label, lp.Path, o.DryRun, out); err != nil {
				return err
			}
		}
	}
	return nil
}
