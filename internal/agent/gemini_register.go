package agent

func init() {
	RegisterRow(2, Schema{Name: "gemini", CLI: CLI{
		Binary: "agy",
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, Autonomous: Native, DebugFile: Native, Direct: Native, Env: Native,
			SystemPrompt: Emulated, AppendSystemPrompt: Emulated, OutputSchema: Native, Persona: Emulated},
		Events: []string{EventStart, EventText, EventResult, EventError}, Cost: true,
		ResumeFormat: "agy --conversation %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
		ConfigureHint: "sign in with `agy` (Google login)", DisplayName: "antigravity", LoginArgv: []string{"agy"},
		ExtraUsage: [][2]string{{"bridge", "ingest files for a large-context task"}},
	}})
	Register(geminiEngine{BaseEngine{"gemini"}})
}
