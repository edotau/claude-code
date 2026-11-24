package assets

// ScienceConfig is the generic claude-science config shipped in the binary.
func ScienceConfig() []byte { return read("files/claude/science.toml") }

// WLPasteProxy is the WSL wl-paste shim that forwards every call to `claude-code clip paste`.
func WLPasteProxy() []byte { return read("files/clipboard/wl-paste-claude.sh") }

// DatabricksClaudeTemplate is the break-glass Claude Code settings seed.
func DatabricksClaudeTemplate() []byte { return read("files/databricks/templates/claude.json") }

// DatabricksCodexTemplate is the break-glass Codex config seed.
func DatabricksCodexTemplate() []byte { return read("files/databricks/templates/codex.toml") }

// TmuxConf is the harness tmux config seeded only where none exists.
func TmuxConf() []byte { return read("files/setup/tmux.conf") }

// VSCodeKeybindingsTemplate is the first-party VS Code keybindings template.
func VSCodeKeybindingsTemplate() []byte { return read("files/ui/vscode/keybindings.template.json") }

// VSCodeMachineSettingsTemplate is the first-party VS Code settings template.
func VSCodeMachineSettingsTemplate() []byte {
	return read("files/ui/vscode/machine-settings.template.json")
}

// PiGuardSource is the Pi guard extension, with __BIN__, __PI_VERSION__ and __BUDGET_MS__ placeholders.
func PiGuardSource() string { return string(read("files/pi/guard/index.ts")) }
