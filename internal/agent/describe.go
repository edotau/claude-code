package agent

// CapFields lists a runner's capability levels in Request-flag order, named as the flags are.
func CapFields(c Caps) []struct {
	Name string
	L    Level
} {
	return []struct {
		Name string
		L    Level
	}{{"model", c.Model}, {"profile", c.Provider}, {"effort", c.Effort}, {"workdir", c.WorkDir}, {"session", c.SessionID},
		{"tools", c.Tools}, {"no-tools", c.NoTools}, {"autonomous", c.Autonomous}, {"enable-memory", c.Memory},
		{"no-persist", c.NoPersist}, {"safe-mode", c.SafeMode}, {"debug-file", c.DebugFile}, {"direct", c.Direct},
		{"system-prompt", c.SystemPrompt}, {"append-system-prompt", c.AppendSystemPrompt}, {"env", c.Env},
		{"output-schema", c.OutputSchema}, {"persona", c.Persona}}
}

// AgentFeatures are the ask capabilities beyond the Request fields: what an agent reports or how it can run.
func AgentFeatures(name string) map[string]Level {
	return map[string]Level{"cost": lv(name == "claude" || name == "gemini"), "background": Native,
		"live": lv(HasLive(name)), "remote": Native}
}

// AgentBinary is the CLI binary the agent drives.
func AgentBinary(name string) string {
	if spec, ok := LookupSpec(name); ok {
		return spec.Binary
	}
	return name
}

// AgentEvents are the typed event kinds the agent's stream can carry; every dispatch brackets them with start and result.
func AgentEvents(name string) []string {
	ev := []string{EventStart, EventText}
	switch name {
	case "claude":
		ev = append(ev, EventToolUse, EventRetry)
	case "codex", "opencode":
		ev = append(ev, EventToolUse, EventToolResult)
	}
	if name != "claude" && IsHubAgent(name) {
		ev = append(ev, EventRetry)
	}
	return append(ev, EventResult, EventError)
}
