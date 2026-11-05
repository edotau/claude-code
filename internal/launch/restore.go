package launch

import (
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// Restore is `configure --restore`: write settings.json whole from the defaults, then render the agent configs.
// Under check, drift reports that any agent config differs from the render.
func Restore(w io.Writer, agents, out string, check, dryRun, forceHomes, noAgents bool) (drift bool, err error) {
	out = restoreOutputPath(out, check, dryRun)
	data, err := settings.RenderLive()
	if err != nil {
		return false, err
	}
	var prepared *render.RenderPlan
	if !noAgents { // --no-agents: lint checks the repo's settings, not this machine's agent configs
		if prepared, drift, err = renderAgentConfigs(w, agents, out, check, dryRun, forceHomes); err != nil {
			return false, err
		}
	}
	if err := restoreSettingsOutput(w, out, check, dryRun, data); err != nil {
		return false, err
	}
	if prepared != nil {
		if err := prepared.Apply(); err != nil {
			return false, err
		}
	}
	return check && drift, nil
}

func restoreOutputPath(out string, check, dryRun bool) string {
	if out == "" && !check && !dryRun { // bare --restore writes the live settings.json
		return settings.SettingsFile()
	}
	return out
}

func restoreSettingsOutput(w io.Writer, out string, check, dryRun bool, data []byte) error {
	switch {
	case check: // --check reports agent-config drift only
		return nil
	case out == "" || dryRun: // a preview: --out names where it WOULD go, never a write
		_, err := w.Write(data)
		return err
	default:
		return restoreSettingsFile(w, out)
	}
}

// restoreSettingsFile snapshots history, then writes the whole render to file and reports both.
func restoreSettingsFile(w io.Writer, file string) error {
	r, err := settings.RestoreSettings(file, agent.NoticeOut)
	if r.Commit != "" {
		fmt.Fprintf(w, "history %s (before restore snapshot)\n", r.Commit)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "restored %s", file)
	if r.Backup != "" {
		fmt.Fprintf(w, " (backup %s)", r.Backup)
	}
	fmt.Fprintln(w)
	return nil
}

// renderAgentConfigs prepares the agent configs; they preview whenever the settings render does.
func renderAgentConfigs(w io.Writer, agents, out string, check, dryRun, forceHomes bool) (*render.RenderPlan, bool, error) {
	preview := dryRun || (out == "" && !check)
	opts := render.RenderOptions{Agents: splitAgents(agents), DryRun: preview, Check: check, Out: w, ForceHomes: forceHomes}
	// Interactive only on a real terminal write: launch/configure callers never reach this function.
	if !preview && !check && tui.StdinIsTerminal() && tui.StdoutIsTerminal() {
		opts.Confirm = func(path string, lost []string) bool {
			return tui.ConfirmKey(fmt.Sprintf("overwrite %s?", path), true)
		}
	}
	return agent.PrepareConfigs(opts)
}

// Revert is `configure --revert [--agent a,b] [--dry-run]`.
func Revert(w io.Writer, agents string, dryRun bool) error {
	return agent.RevertConfigs(render.RenderOptions{Agents: splitAgents(agents), DryRun: dryRun, Out: w})
}

func splitAgents(agents string) []string {
	if agents == "" {
		return nil
	}
	return strings.Split(agents, ",")
}
