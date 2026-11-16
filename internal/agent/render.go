package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// DefaultAgents is every agent generate renders when agentconfig.RenderOptions.Agents is empty.
var DefaultAgents = []string{agentid.Codex, agentid.OpenCode, agentid.Gemini, agentid.Copilot}

// RenderConfigs renders every selected agent's config and reports whether anything differs (Check) or would be written.
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
	agents := o.Agents
	if len(agents) == 0 {
		agents = DefaultAgents
	}
	out := o.Out
	if out == nil {
		out = os.Stdout
	}
	home, err := agentconfig.HomeOr(o.Home)
	if err != nil {
		return nil, false, fmt.Errorf("generate: %w", err)
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
	drift, err = prepareSelectedConfigs(context.Background(), agents, reg, sel, home, o, out)
	if err != nil {
		return nil, drift, err
	}
	return p, drift, nil
}

// SyncConfigs renders each agent's config alone and returns those that differ; an absent agent home is skipped.
func SyncConfigs(o agentconfig.RenderOptions) (changed []string, skipped bool, err error) {
	if skipConfigHomes(o) {
		return nil, true, nil
	}
	agents := o.Agents
	if len(agents) == 0 {
		agents = DefaultAgents
	}
	note := o.Out
	if note == nil {
		note = io.Discard
	}
	for _, a := range agents {
		if dir := absentHome(a, o); dir != "" {
			fmt.Fprintf(note, "sync: skipped %s: %s does not exist\n", a, dir)
			continue
		}
		probe := o
		probe.Agents, probe.Check, probe.Out = []string{a}, true, io.Discard
		drift, rerr := RenderConfigs(probe)
		if rerr == nil && drift && !o.Check && !o.DryRun {
			write := o
			write.Agents, write.Out = []string{a}, io.Discard
			_, rerr = RenderConfigs(write)
		}
		if rerr != nil {
			err = errors.Join(err, fmt.Errorf("%s: %w", a, rerr))
			continue
		}
		if drift {
			changed = append(changed, a)
		}
	}
	return changed, false, err
}

// absentHome is the missing directory a known agent's config files live under (its home); "" when present or unknown.
func absentHome(a string, o agentconfig.RenderOptions) string {
	if knownAgents([]string{a}) != nil {
		return ""
	}
	files, err := SchemaFor(a).Eng().ConfigPaths(o.Home)
	userHome, herr := agentconfig.HomeOr(o.Home)
	if err != nil || herr != nil || len(files) == 0 {
		return ""
	}
	dir := filepath.Dir(files[0].Path)
	if rel, err := filepath.Rel(userHome, files[0].Path); err == nil && !strings.HasPrefix(rel, "..") {
		dir = filepath.Join(userHome, strings.SplitN(filepath.ToSlash(rel), "/", 2)[0])
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return dir
	}
	return ""
}

func skipConfigHomes(o agentconfig.RenderOptions) bool {
	return !o.ForceHomes && !paths.ConfigDirIsDefault() && !paths.ConfigDirIsInstall()
}

func prepareSelectedConfigs(ctx context.Context, agents []string, reg *providers.Registry, sel providers.Selection, home string, o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	if err := knownAgents(agents); err != nil {
		return false, err
	}
	var drift bool
	for _, name := range agents {
		d, err := SchemaFor(name).Eng().Render(ctx, reg, sel, home, o, out)
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

// RevertConfigs restores each selected agent's files to their newest backup, so a second revert steps one version back.
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
