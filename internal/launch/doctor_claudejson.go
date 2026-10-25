package launch

import (
	"os"
	"path/filepath"
)

// strayClaudeJSON flags ~/.claude/.claude.json: claude reads it instead of ~/.claude.json under CLAUDE_CONFIG_DIR=~/.claude.
func strayClaudeJSON() (ok bool, detail string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return true, "home unresolved"
	}
	stub := filepath.Join(home, ".claude", ".claude.json")
	if _, err := os.Stat(stub); err != nil {
		return true, "none (~/.claude.json is the only one)"
	}
	return false, "stray " + stub + "; move it aside (mv " + stub + "{,.bak})"
}
