package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// opencodeRulesOn adds the portable core to cfg as `instructions`, minus CLAUDE.md, which opencode reads natively.
func opencodeRulesOn(cfg map[string]any) {
	core, err := project.PortableCore(paths.ConfigDir())
	if err != nil {
		fmt.Fprintln(agentconfig.CatalogWarn, "opencode:", err)
		return
	}
	if len(core) > 1 {
		cfg["instructions"] = core[1:]
	}
}

// opencodeCommands projects commands/*.md as opencode `command`s; those allowing Claude's Agent tool are skipped.
func opencodeCommands() map[string]any {
	entries, err := os.ReadDir(filepath.Join(paths.ConfigDir(), "commands"))
	if err != nil {
		return nil
	}
	out := map[string]any{}
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || e.IsDir() {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(paths.ConfigDir(), "commands", e.Name()))
		if rerr != nil {
			continue
		}
		fm, body := textutil.ParseFrontmatter(string(raw))
		if tools, _ := fm["allowed-tools"].(string); slices.Contains(textutil.SplitCSV(tools), "Agent") {
			continue
		}
		cmd := map[string]any{"template": body}
		if d, _ := fm["description"].(string); d != "" {
			cmd["description"] = d
		}
		if fm["context"] == "fork" {
			cmd["subtask"] = true // a forked context is opencode's subtask
		}
		out[name] = cmd
	}
	return out
}

// opencodeCommandsOn adds the projected commands to cfg as opencode `command`.
func opencodeCommandsOn(cfg map[string]any) {
	if cmds := opencodeCommands(); len(cmds) > 0 {
		cfg["command"] = cmds
	}
}
