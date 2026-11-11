package configure

import (
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// Restore is `configure --restore`. With agents named or forceHomes (--agent/--agents) it renders only the agent
// configs and never touches settings.json; otherwise it resets settings.json (backup first), then renders the agent
// configs unless noAgents. out replaces the settings.json path.
func Restore(w io.Writer, agents, out string, forceHomes, noAgents bool) error {
	if agents != "" || forceHomes {
		return renderAgentConfigs(w, agents, forceHomes)
	}
	if out == "" {
		out = settings.SettingsFile()
	}
	backup, err := settings.ResetSettings(out)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "restored %s", out)
	if backup != "" {
		fmt.Fprintf(w, " (backup %s)", backup)
	}
	fmt.Fprintln(w)
	if noAgents {
		return nil
	}
	return renderAgentConfigs(w, "", false)
}

// renderAgentConfigs renders the agent configs, asking before an overwrite on a real terminal.
func renderAgentConfigs(w io.Writer, agents string, forceHomes bool) error {
	opts := agentconfig.RenderOptions{Agents: splitAgents(agents), Out: w, ForceHomes: forceHomes}
	if tui.StdinIsTerminal() && tui.StdoutIsTerminal() {
		opts.Confirm = func(path string, lost []string) bool {
			return tui.ConfirmKey(fmt.Sprintf("overwrite %s?", path), true)
		}
	}
	prepared, _, err := agent.PrepareConfigs(opts)
	if err != nil || prepared == nil {
		return err
	}
	return prepared.Apply()
}

// Revert is `configure --revert [--agent a,b] [--dry-run]`.
func Revert(w io.Writer, agents string, dryRun bool) error {
	return agent.RevertConfigs(agentconfig.RenderOptions{Agents: splitAgents(agents), DryRun: dryRun, Out: w})
}

func splitAgents(agents string) []string {
	if agents == "" {
		return nil
	}
	return strings.Split(agents, ",")
}
