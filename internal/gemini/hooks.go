package gemini

import (
	"io"
	"path/filepath"

	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/settings"
)

// agyHookName is the one named hook in agy's hooks.json the harness owns; every render replaces it.
const agyHookName = "claude-code"

// agyGateMatcher is the agy tool names the permission gate judges (run_command, read_file, write_file, edit_file).
const agyGateMatcher = "run_command|read_file|write_file|edit_file"

// renderGeminiHooks merges the harness's agy PreToolUse hooks into ~/.gemini/config/hooks.json, keeping every other hook.
func renderGeminiHooks(home string, o render.RenderOptions, out io.Writer) (bool, error) {
	var hooks []any
	for _, s := range settings.SpecsFor("gemini", "native") {
		hooks = append(hooks, map[string]any{
			"type": "command", "command": settings.AgentCommand("gemini", s), "timeout": s.Seconds(),
		})
	}
	path := filepath.Join(home, ".gemini", "config", "hooks.json")
	return render.CommitJSONObject(path, o, out, "gemini-hooks", func(obj map[string]any, _ bool) bool {
		obj[agyHookName] = map[string]any{"PreToolUse": []any{map[string]any{"matcher": agyGateMatcher, "hooks": hooks}}}
		return true
	})
}
