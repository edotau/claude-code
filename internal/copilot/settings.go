package copilot

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/render"
)

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
	return render.CommitJSONObject(path, o, out, "copilot-settings", func(obj map[string]any, _ bool) bool {
		obj["defaultPermissionMode"] = mode
		return true
	})
}
