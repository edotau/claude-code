package agent

// CapFields lists a runner's capability levels in RunOptions-flag order, named as the flags are.
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
