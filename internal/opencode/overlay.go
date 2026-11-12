package opencode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
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

// opencodeRulesOn adds the portable core to cfg as `instructions`, minus CLAUDE.md (read natively) if it's the root
func opencodeRulesOn(cfg map[string]any) {
	core, err := project.PortableCore(paths.ConfigDir())
	if err != nil {
		fmt.Fprintln(agentconfig.CatalogWarn, "opencode:", err)
		return
	}
	if len(core) > 0 && filepath.Base(core[0]) == "CLAUDE.md" {
		core = core[1:]
	}
	if len(core) > 0 {
		cfg["instructions"] = core
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
		if slices.Contains(allowedTools(fm["allowed-tools"]), "Agent") {
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

// allowedTools is a command's allowed-tools frontmatter: a comma string or a (flow or block) list.
func allowedTools(v any) []string {
	switch t := v.(type) {
	case string:
		return textutil.SplitCSV(t)
	case []string:
		return t
	}
	return nil
}

// opencodeCommandsOn adds the projected commands to cfg as opencode `command`.
func opencodeCommandsOn(cfg map[string]any) {
	if cmds := opencodeCommands(); len(cmds) > 0 {
		cfg["command"] = cmds
	}
}
