// Package agent (project_*.go) projects agents/ into every agent's native subagent dir (tracked under dist/), links skills/
// into ~/.agents/skills (plus agy's own GeminiSkillsDir), and renders each agent's global instruction file
// (~/.codex/AGENTS.md, ~/.gemini/GEMINI.md).
package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// displayPath renders a path home-relative (~/...) when possible, else absolute.
func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return path
}
