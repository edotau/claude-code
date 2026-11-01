package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agentconfig/render"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
)

// Copilot hooks load from the user directory; plugin preToolUse hooks do not fire.

func renderCopilot(reg *providers.Registry, sel providers.Selection, home string, o render.RenderOptions, out io.Writer) (bool, error) {
	body, err := CopilotJSON()
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")
	drift, err := render.RenderHooksFile(path, body, mergeCopilotHooks, o, out, "copilot-hooks")
	if err != nil {
		return drift, err
	}
	sd, err := renderCopilotSettings(home, o, out)
	if err != nil {
		return drift, err
	}
	drift = drift || sd
	ctx := context.Background()
	target, note, err := copilotSchema().ResolveTarget(reg, sel, false)
	if err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if _, _, err := copilotSchema().Configure(ctx, target, ""); err != nil {
		fmt.Fprintf(out, "copilot: skipped: %v\n", err)
		return drift, nil
	}
	fmt.Fprintf(out, "copilot: launch-time env only; route = %s (%s)\n", target.Provider.Name, target.Model)
	return drift, nil
}

// renderCopilotSettings merges defaultPermissionMode from the agent spec into ~/.copilot/settings.json,
// keeping every other key untouched; a non-JSON existing file is skipped, never clobbered.
func renderCopilotSettings(home string, o render.RenderOptions, out io.Writer) (bool, error) {
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
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		fmt.Fprintf(out, "copilot-settings: skipped: cannot read %s: %v\n", path, readErr)
		return false, nil
	}
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
	return render.CommitFile(path, raw, append(body, '\n'), existed, o, out, "copilot-settings")
}
