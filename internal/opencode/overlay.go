package opencode

import (
	"fmt"
	"io"

	"github.com/edotau/claude-code/internal/agentconfig"
)

// --- opencode: a tracked overlay (state/harness/opencode/{opencode,tui}.json), never opencode's own
// ~/.config/opencode files; the harness only writes those for the custom theme (see internal/ui/theme). ---

// renderOpenCodeOverlay writes the tracked opencode.json/tui.json overlay templates.
func renderOpenCodeOverlay(o agentconfig.RenderOptions, out io.Writer) (bool, error) {
	cfgPath, cfgBody, tuiPath, tuiBody, err := agentconfig.OpencodeOverlayRendered()
	if err != nil {
		return false, fmt.Errorf("opencode: %w", err)
	}
	cfgRaw, _, err := agentconfig.ReadExisting(cfgPath)
	if err != nil {
		return false, fmt.Errorf("opencode: %w", err)
	}
	tuiRaw, _, err := agentconfig.ReadExisting(tuiPath)
	if err != nil {
		return false, fmt.Errorf("opencode: %w", err)
	}
	cfgChanged, err := agentconfig.CommitOverlayFile(cfgPath, cfgRaw, cfgBody, o, out, "opencode")
	if err != nil {
		return false, err
	}
	tuiChanged, err := agentconfig.CommitOverlayFile(tuiPath, tuiRaw, tuiBody, o, out, "opencode-tui")
	if err != nil {
		return false, err
	}
	return cfgChanged || tuiChanged, nil
}
