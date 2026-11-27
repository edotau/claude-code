package assets

import "strings"

// ScienceConfig is the generic claude-science config shipped in the binary.
func ScienceConfig() []byte { return read("claude/science.toml") }

// WLPasteProxy is the WSL wl-paste shim that forwards every call to `claude-code clip paste`.
func WLPasteProxy() []byte { return read("clipboard/wl-paste-claude.sh") }

// DatabricksClaudeTemplate is the break-glass Claude Code settings seed.
func DatabricksClaudeTemplate() []byte { return read("databricks/templates/claude.json") }

// DatabricksCodexTemplate is the break-glass Codex config seed.
func DatabricksCodexTemplate() []byte { return read("databricks/templates/codex.toml") }

// TmuxConf is the harness tmux config seeded only where none exists.
func TmuxConf() []byte { return read("setup/tmux.conf") }

// PiSystemPrompt is the short harness system prompt of a Pi tools run.
func PiSystemPrompt() string { return strings.TrimSpace(string(read("pi/system-prompt.txt"))) }

// PiGuardSource is the Pi guard extension, with __BIN__, __PI_VERSION__ and __BUDGET_MS__ placeholders.
func PiGuardSource() string { return string(read("pi/guard/index.ts")) }
