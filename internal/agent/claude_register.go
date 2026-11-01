package agent

func init() {
	RegisterRow(0, Schema{Name: "claude", CLI: CLI{
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, NoTools: Native, Autonomous: Native, NoPersist: Native, SafeMode: Native, DebugFile: Native,
			SystemPrompt: Native, AppendSystemPrompt: Native, Env: Native, OutputSchema: Native, Persona: Native},
		Events: []string{EventStart, EventText, EventToolUse, EventRetry, EventResult, EventError},
		Cost:   true, ConfigureHint: "run `claude-code configure agents`", LoginArgv: []string{"claude", "auth", "login"},
	}})
}
