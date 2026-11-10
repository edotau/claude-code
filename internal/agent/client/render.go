// render.go renders agent configs and hooks without launching a session.
package client

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"

	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// DefaultAgents is every agent generate renders when agentrender.RenderOptions.Agents is empty.
var DefaultAgents = []string{agentid.Codex, agentid.OpenCode, agentid.Gemini, agentid.Copilot}

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

func skipConfigHomes(o agentrender.RenderOptions) bool {
	return !o.ForceHomes && !paths.ConfigDirIsDefault() && !paths.ConfigDirIsInstall()
}

func prepareSelectedConfigs(agents []string, reg *providers.Registry, sel providers.Selection, home string, o agentrender.RenderOptions, out io.Writer) (bool, error) {
	if err := knownAgents(agents); err != nil {
		return false, err
	}
	var drift bool
	for _, name := range agents {
		d, err := agent.SchemaFor(name).Eng().Render(reg, sel, home, o, out)
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
func RevertConfigs(o agentrender.RenderOptions) error {
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
		files, err := agent.SchemaFor(a).Eng().ConfigPaths(o.Home)
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

// RosterRow is one agent in `agents`: its binary, availability, capability levels and event kinds.
type RosterRow struct {
	Name      string            `json:"name"`
	Binary    string            `json:"binary"`
	Available bool              `json:"available"`
	Reason    string            `json:"reason,omitempty"`
	Caps      map[string]string `json:"caps"`
	Events    []string          `json:"events"`
	Defaults  agent.Permissions `json:"permissions"` // the resolved default dispatch policy; {} = none, the tier flags decide
	Levels    agent.Caps        `json:"-"`
	shown     []string
}

// rosterShown are the capabilities the text roster lists beside each agent.
var rosterShown = []string{"model", "profile", "effort", "workdir", "session", "tools", "system-prompt",
	"append-system-prompt", "env", "output-schema"}

// PrintRoster prints RosterRows as the JSON array of rows, or one text line each.
func PrintRoster(format string) int {
	rows := RosterRows()
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("agents", rows)
	}
	for _, r := range rows {
		fmt.Println(r.Line())
	}
	return 0
}

// RosterRows describes every registered agent, probing each one's availability.
func RosterRows() []RosterRow {
	var rows []RosterRow
	for _, r := range agent.Engines() {
		row := RosterRow{Available: true}
		if err := r.Available(); err != nil {
			row.Available, row.Reason = false, err.Error()
		}
		s, _ := agent.LookupSchema(r.Name())
		row.Caps = map[string]string{}
		for _, f := range CapFields(s.CLI.Caps) {
			row.Caps[f.Name] = f.L.String()
			if f.L != agent.None && slices.Contains(rosterShown, f.Name) {
				row.shown = append(row.shown, f.Name+":"+f.L.String())
			}
		}
		row.Caps["cost"], row.Caps["live"] = agent.None.String(), agent.None.String()
		if s.CLI.Cost {
			row.Caps["cost"] = agent.Native.String()
		}
		if s.HasLive() {
			row.Caps["live"] = agent.Native.String()
		}
		row.Caps["background"], row.Caps["remote"] = agent.Native.String(), agent.Native.String()
		row.Binary = s.CLI.Binary
		if !s.External() {
			row.Binary = s.Name
		}
		row.Name, row.Events, row.Levels = s.Name, s.CLI.Events, s.CLI.Caps
		var err error
		if row.Defaults, err = agent.DefaultPermissions(s.Name); err != nil { // checkRun refuses every dispatch on it
			row.Available, row.Reason = false, err.Error()
		}
		rows = append(rows, row)
	}
	return rows
}

// Line is the text roster row: availability mark, name, the shown capabilities and the unavailable reason.
func (r RosterRow) Line() string {
	mark := "✓"
	if !r.Available {
		mark = "✗"
	}
	return fmt.Sprintf("%s %-9s %s %s", mark, r.Name, strings.Join(r.shown, ","), r.Reason)
}

// CapFields lists an agent's capability levels in RunOptions-flag order, named as the flags are.
func CapFields(c agent.Caps) []struct {
	Name string
	L    agent.Level
} {
	return []struct {
		Name string
		L    agent.Level
	}{{"model", c.Model}, {"profile", c.Provider}, {"effort", c.Effort}, {"workdir", c.WorkDir}, {"session", c.SessionID},
		{"tools", c.Tools}, {"no-tools", c.NoTools}, {"autonomous", c.Autonomous}, {"fleet", c.Fleet}, {"enable-memory", c.Memory},
		{"no-persist", c.NoPersist}, {"safe-mode", c.SafeMode}, {"debug-file", c.DebugFile}, {"direct", c.Direct},
		{"system-prompt", c.SystemPrompt}, {"append-system-prompt", c.AppendSystemPrompt}, {"env", c.Env},
		{"output-schema", c.OutputSchema}, {"persona", c.Persona}}
}
