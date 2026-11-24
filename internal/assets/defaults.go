package assets

// ProviderDefaults is the built-in provider registry source.
func ProviderDefaults() []byte { return read("files/providers/defaults.json") }

// ModelCatalog is the canonical model catalog source.
func ModelCatalog() []byte { return read("files/models/catalog.json") }

// RouteTable is the built-in routing table source.
func RouteTable() []byte { return read("files/router/table.json") }

// ProxyTeams is the built-in proxy team schema source.
func ProxyTeams() []byte { return read("files/proxy/teams.json") }

// ShellGuards is the shell guard vocabulary source.
func ShellGuards() []byte { return read("files/shell/guards.json") }

// AgentDefaults is the built-in external agent configuration.
func AgentDefaults() []byte { return read("files/agentconfig/agents.json") }

// OverlaySchemaSnapshot is the checked-in agent overlay schema.
func OverlaySchemaSnapshot() []byte { return read("files/agentconfig/overlay.schema.json") }

// OpencodeConfigTemplate is the OpenCode configuration template.
func OpencodeConfigTemplate() []byte { return read("files/agentconfig/template/opencode.json") }

// OpencodeTUITemplate is the OpenCode TUI template.
func OpencodeTUITemplate() []byte { return read("files/agentconfig/template/opencode-tui.json") }

// SettingsAutoMode is the auto mode settings source.
func SettingsAutoMode() []byte { return read("files/settings/automode.json") }

// SettingsTemplate is the settings seed template.
func SettingsTemplate() []byte { return read("files/settings/template.json") }

// SettingsSchema is the settings validation schema.
func SettingsSchema() []byte { return read("files/settings/claude-code-settings.schema.json") }
