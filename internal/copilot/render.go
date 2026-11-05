package copilot

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agentconfig/render"
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
