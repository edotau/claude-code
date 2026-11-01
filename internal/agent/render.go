// render.go renders agent configs and hooks without launching a session.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/edotau/claude-code/internal/agentconfig"
	"io"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// DefaultAgents is every agent generate renders when RenderOptions.Agents is empty.
var DefaultAgents = []string{"codex", "opencode", "gemini", "copilot"}

// RenderOptions controls one generate pass.
type RenderOptions struct {
	Agents     []string  // agents to render; default DefaultAgents
	DryRun     bool      // print the rendered blocks; write nothing
	Check      bool      // report drift; never writes
	Home       string    // overrides os.UserHomeDir() for agent config paths (tests)
	Out        io.Writer // status lines; nil -> os.Stdout
	ForceHomes bool      // force agent-config renders even when CLAUDE_CONFIG_DIR is not the default ~/.claude
	// Confirm asks whether to overwrite path given its lost hand-edited lines; nil means write and warn.
	Confirm func(path string, lost []string) bool
	// PrepareConfigs' write queue: CommitFile and the JSONC retire append to it, Apply runs it.
	staged *[]func() error
}

// RenderPlan holds ordered agent writes after all selected renders have validated.
type RenderPlan struct {
	writes []func() error
}

// Apply performs the staged writes in order, stopping at the first error.
func (p *RenderPlan) Apply() error {
	for _, write := range p.writes {
		if err := write(); err != nil {
			return err
		}
	}
	return nil
}

// RenderConfigs renders every selected agent's config and reports whether anything
// differs from what is on disk (Check) or would be written (DryRun never writes either way).
func RenderConfigs(o RenderOptions) (bool, error) {
	p, drift, err := PrepareConfigs(o)
	if err != nil {
		return drift, err
	}
	return drift, p.Apply()
}

// PrepareConfigs validates all selected renders without writing; dry-run/check output remains immediate.
func PrepareConfigs(o RenderOptions) (*RenderPlan, bool, error) {
	p := &RenderPlan{}
	o.staged = &p.writes
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

// CommitFile is the shared dry-run/check/write decision once a renderer has the next byte slice.
func CommitFile(path string, raw, next []byte, existed bool, o RenderOptions, out io.Writer, label string) (bool, error) {
	changed := !bytes.Equal(next, raw)
	switch {
	case o.DryRun:
		fmt.Fprintf(out, "--- %s (%s) ---\n%s\n", path, label, next)
		return changed, nil
	case o.Check:
		if changed {
			fmt.Fprintf(out, "%s: drift at %s\n", label, path)
		}
		lost, err := lostLinesFor(label, path, raw, next)
		if err != nil {
			return changed, err
		}
		if len(lost) > 0 {
			fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) not in this render:\n", label, path, len(lost))
			printLostLines(out, lost)
		}
		return changed, nil
	case !changed:
		if ops := fileOps[label]; ops.LoadBaseline != nil {
			base, err := ops.LoadBaseline(path)
			if err != nil {
				return false, err
			}
			if base == nil && existed {
				*o.staged = append(*o.staged, func() error { return ops.SaveBaseline(path, next) })
			}
			fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
			return false, nil
		}
		// Seed a missing baseline from an unchanged file, or later hand edits go undetected.
		if _, err := os.Stat(baselinePath(label, path)); os.IsNotExist(err) && existed {
			_ = writeBaseline(label, path, next)
		}
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return false, nil
	}
	*o.staged = append(*o.staged, func() error { return commitWrite(path, raw, next, existed, o, out, label) })
	return true, nil
}

// commitWrite runs the lost-lines check (prompting or warning), writes path, and refreshes its
// baseline; it is the body of every CommitFile staged write.
func commitWrite(path string, raw, next []byte, existed bool, o RenderOptions, out io.Writer, label string) error {
	lost, err := lostLinesFor(label, path, raw, next)
	if err != nil {
		return err
	}
	if len(lost) > 0 {
		if o.Confirm != nil {
			fmt.Fprintf(out, "%s: %s has %d hand-edited line(s) that would be lost:\n", label, path, len(lost))
			printLostLines(out, lost)
			if !o.Confirm(path, lost) {
				fmt.Fprintf(out, "%s: kept %s (your edits)\n", label, path)
				return nil
			}
		} else {
			fmt.Fprintf(out, "%s: overwrote %d hand-edited line(s) in %s; `claude-code configure --revert` restores\n",
				label, len(lost), path)
			printLostLines(out, lost)
		}
	}
	write := backupAndWrite
	if ops := fileOps[label]; ops.Write != nil {
		write = ops.Write
	}
	if err := write(path, raw, next, existed); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := writeBaseline(label, path, next); err != nil {
		fmt.Fprintf(out, "%s: baseline: %v (next run can't detect hand edits)\n", label, err)
	}
	fmt.Fprintf(out, "%s: wrote %s\n", label, path)
	return nil
}

// --- opencode: a tracked overlay (state/harness/opencode/{opencode,tui}.json), never opencode's own
// ~/.config/opencode files; the harness only writes those for the custom theme (see internal/ui/theme). ---

// renderOpenCodeOverlay writes the tracked opencode.json/tui.json overlay templates.
func renderOpenCodeOverlay(o RenderOptions, out io.Writer) (bool, error) {
	cfgPath, cfgBody, tuiPath, tuiBody, err := agentconfig.OpencodeOverlayRendered()
	if err != nil {
		return false, fmt.Errorf("opencode: %w", err)
	}
	cfgRaw, _ := os.ReadFile(cfgPath)
	tuiRaw, _ := os.ReadFile(tuiPath)
	cfgChanged, err := commitOverlayFile(cfgPath, cfgRaw, cfgBody, o, out, "opencode")
	if err != nil {
		return false, err
	}
	tuiChanged, err := commitOverlayFile(tuiPath, tuiRaw, tuiBody, o, out, "opencode-tui")
	if err != nil {
		return false, err
	}
	return cfgChanged || tuiChanged, nil
}

// commitOverlayFile is CommitFile's dry-run/check/write decision, minus the lost-hand-edited-lines
// check and baseline tracking: the overlay is never hand-edited, so there is nothing to lose.
func commitOverlayFile(path string, raw, next []byte, o RenderOptions, out io.Writer, label string) (bool, error) {
	changed := !bytes.Equal(raw, next)
	switch {
	case o.DryRun:
		fmt.Fprintf(out, "--- %s (%s) ---\n%s\n", path, label, next)
		return changed, nil
	case o.Check:
		if changed {
			fmt.Fprintf(out, "%s: drift at %s\n", label, path)
		}
		return changed, nil
	case !changed:
		fmt.Fprintf(out, "%s: %s unchanged\n", label, path)
		return false, nil
	}
	*o.staged = append(*o.staged, func() error {
		if _, err := paths.WriteIfChanged(path, next, 0o600); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		fmt.Fprintf(out, "%s: wrote %s\n", label, path)
		return nil
	})
	return true, nil
}

// Copilot hooks load from the user directory; plugin preToolUse hooks do not fire.

func renderCopilot(reg *providers.Registry, sel providers.Selection, home string, o RenderOptions, out io.Writer) (bool, error) {
	body, err := CopilotJSON()
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")
	drift, err := renderHooksFile(path, body, mergeCopilotHooks, o, out, "copilot-hooks")
	if err != nil {
		return drift, err
	}
	sd, err := renderCopilotSettings(home, o, out)
	if err != nil {
		return drift, err
	}
	drift = drift || sd
	ctx := context.Background()
	target, note, err := schemaFor("copilot").ResolveTarget(reg, sel, false)
	if err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if _, _, err := schemaFor("copilot").Configure(ctx, target, ""); err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	fmt.Fprintf(out, "copilot: launch-time env only; route = %s (%s)\n", target.Provider.Name, target.Model)
	return drift, nil
}

// renderCopilotSettings merges defaultPermissionMode from the agent spec into ~/.copilot/settings.json,
// keeping every other key untouched; a non-JSON existing file is skipped, never clobbered.
func renderCopilotSettings(home string, o RenderOptions, out io.Writer) (bool, error) {
	cfg, err := agentconfig.LoadConfig()
	if err != nil {
		fmt.Fprintf(out, "copilot-settings: skipped: %v\n", err)
		return false, nil
	}
	mode := cfg.Agent("copilot").Permission.DefaultPermissionMode
	if mode == "" {
		return false, nil
	}
	path := filepath.Join(home, ".copilot", "settings.json")
	raw, readErr := os.ReadFile(path)
	existed := readErr == nil
	next := map[string]any{}
	if existed {
		if err := json.Unmarshal(raw, &next); err != nil {
			fmt.Fprintf(out, "copilot-settings: skipped: %s is not plain JSON: %v\n", path, err)
			return false, nil
		}
		if next == nil { // "null" decodes without error but leaves next nil, not an empty object
			fmt.Fprintf(out, "copilot-settings: skipped: %s is not plain JSON: not an object\n", path)
			return false, nil
		}
	}
	next["defaultPermissionMode"] = mode
	body, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return false, fmt.Errorf("copilot-settings: %w", err)
	}
	return CommitFile(path, raw, append(body, '\n'), existed, o, out, "copilot-settings")
}
