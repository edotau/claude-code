package configure

import (
	"cmp"
	"fmt"
	"io"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// Restore resets settings.json (backup first), then renders agent configs; --out skips the render, --agents the reset.
func Restore(w io.Writer, agents, out string, forceHomes bool) error {
	if agents != "" || forceHomes {
		return confirmAndRender(w, agents, forceHomes)
	}
	target := cmp.Or(out, settings.SettingsFile())
	backup, err := settings.ResetSettings(target)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "restored %s", target)
	if backup != "" {
		fmt.Fprintf(w, " (backup %s)", backup)
	}
	fmt.Fprintln(w)
	if out != "" {
		return nil // "this file instead": the live agent configs stay untouched
	}
	return confirmAndRender(w, "", false)
}

// confirmAndRender renders the agent configs, asking before an overwrite on a real terminal.
func confirmAndRender(w io.Writer, agents string, forceHomes bool) error {
	opts := agentconfig.RenderOptions{Agents: textutil.SplitCSV(agents), Out: w, ForceHomes: forceHomes}
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
	return agent.RevertConfigs(agentconfig.RenderOptions{Agents: textutil.SplitCSV(agents), DryRun: dryRun, Out: w})
}
