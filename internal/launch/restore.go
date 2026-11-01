package launch

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// Restore is `configure --restore`: write settings.json whole from the defaults, then render the agent configs.
// Under check, drift reports that any rendered file differs from the render.
func Restore(w io.Writer, agents, out string, check, dryRun, forceHomes, noAgents bool) (drift bool, err error) {
	if out == "" && !check && !dryRun { // bare --restore writes the live settings.json
		out = SettingsFile()
	}
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
	var outDrift bool
	if out != "" && !check && !dryRun {
		err = restoreSettingsFile(w, out)
	} else {
		outDrift, err = writeSettingsOutput(w, out, check, dryRun, data)
	}
	if err != nil {
		return false, err
	}
	winDrift, err := writeWindowsSettingsOutput(w, out, check)
	if err != nil {
		return false, err
	}
	if prepared != nil {
		if err := prepared.Apply(); err != nil {
			return false, err
		}
	}
	return check && (drift || outDrift || winDrift), nil
}

// writeWindowsSettingsOutput reports Windows settings.json drift under --check; the Windows file path
// is fixed (not overridable via --out), and only `install` writes it, so bare/--dry-run generate stay
// read-only here too, matching settings.json's own preview-only default.
func writeWindowsSettingsOutput(w io.Writer, out string, check bool) (bool, error) {
	if out != "" || !check {
		return false, nil
	}
	return checkLiveWindowsSettings(w)
}

// checkLiveWindowsSettings prints the Windows drift findings and reports whether it has drifted;
// ok=false (no Windows profile resolved) prints nothing and is not drift.
func checkLiveWindowsSettings(w io.Writer) (bool, error) {
	drift, ok, err := CheckWindowsSettingsDrift()
	if err != nil || !ok {
		return false, err
	}
	for _, l := range drift.LinesLabeled("settings (windows)") {
		fmt.Fprintln(w, l)
	}
	return drift.Drifted(), nil
}

// restoreSettingsFile snapshots history, then writes the whole render to file and reports both.
func restoreSettingsFile(w io.Writer, file string) error {
	r, err := RestoreSettings(file)
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

// writeSettingsOutput writes the rendered settings per --out/--check/--dry-run and reports drift.
func writeSettingsOutput(w io.Writer, out string, check, dryRun bool, data []byte) (bool, error) {
	switch {
	case out != "" && check: // drift report only: the rendered settings against the --out file
		if cur, err := os.ReadFile(out); err != nil || !bytes.Equal(cur, data) {
			fmt.Fprintf(w, "settings: %s has drifted from the render\n", out)
			return true, nil
		}
		return false, nil
	case out == "" && check: // no --out: check the live settings.json, settings.local.json and nested config
		return checkLiveSettings(w, data)
	case out == "" || dryRun: // a preview: --out names where it WOULD go, never a write
		_, err := w.Write(data)
		return false, err
	default:
		backup, err := settings.WriteWithBackup(out, data)
		if err != nil {
			return false, err
		}
		fmt.Fprintf(w, "wrote %s", out)
		if backup != "" {
			fmt.Fprintf(w, " (backup %s)", backup)
		}
		fmt.Fprintln(w)
		return false, nil
	}
}

// checkLiveSettings prints the drift findings for settings.json and reports whether it has drifted.
func checkLiveSettings(w io.Writer, rendered []byte) (bool, error) {
	drift, err := CheckSettingsDriftWith(rendered)
	if err != nil {
		return false, err
	}
	for _, l := range drift.Lines() {
		fmt.Fprintln(w, l)
	}
	return drift.Drifted(), nil
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
