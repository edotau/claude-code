package opencode

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// opencodeRuleFiles is the sorted rules/**/*.md without `paths:` frontmatter: what Claude Code always loads.
func opencodeRuleFiles() []string {
	root := filepath.Join(paths.ConfigDir(), "rules")
	var files []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if fm, _ := textutil.ParseFrontmatter(string(raw)); fm["paths"] == nil {
			files = append(files, p)
		}
		return nil
	})
	slices.Sort(files)
	return files
}

// opencodeRulesOn adds the always-on rules to cfg as opencode `instructions`.
func opencodeRulesOn(cfg map[string]any) {
	if files := opencodeRuleFiles(); len(files) > 0 {
		cfg["instructions"] = files
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
