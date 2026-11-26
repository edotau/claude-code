package assets

// ProviderDefaults is the built-in provider registry source.
func ProviderDefaults() []byte { return read("providers/defaults.json") }

// ModelCatalog is the canonical model catalog source.
func ModelCatalog() []byte { return read("models/catalog.json") }

// RouteTable is the built-in routing table source.
func RouteTable() []byte { return read("router/table.json") }

// JevTier is the Open-Jev tier question sent by the shadow router signal.
func JevTier() []byte { return read("router/jev-tier.json") }

// ProxyTeams is the built-in proxy team schema source.
func ProxyTeams() []byte { return read("proxy/teams.json") }

// ShellGuards is the shell guard vocabulary source.
func ShellGuards() []byte { return read("shell/guards.json") }

// AgentDefaults is the built-in external agent configuration.
func AgentDefaults() []byte { return read("agentconfig/agents.json") }

// OverlaySchemaSnapshot is the checked-in agent overlay schema.
func OverlaySchemaSnapshot() []byte { return read("agentconfig/overlay.schema.json") }

// OpencodeConfigTemplate is the OpenCode configuration template.
func OpencodeConfigTemplate() []byte { return read("agentconfig/template/opencode.json") }

// OpencodeTUITemplate is the OpenCode TUI template.
func OpencodeTUITemplate() []byte { return read("agentconfig/template/opencode-tui.json") }

// SettingsAutoMode is the auto mode settings source.
func SettingsAutoMode() []byte { return read("settings/automode.json") }

// SettingsTemplate is the settings seed template.
func SettingsTemplate() []byte { return read("settings/template.json") }

// SettingsSchema is the settings validation schema.
func SettingsSchema() []byte { return read("settings/claude-code-settings.schema.json") }
