package agent

import "github.com/edotau/claude-code/internal/agent/vendors"

// opencodeSystemAgent is the custom agent whose prompt carries --system-prompt for opencode.
const opencodeSystemAgent = "harness-system"

// opencodeSystemPrompt defines a primary agent with prompt in vendors.OpencodeConfigEnv (merged into any config
// already there) and selects it with --agent, the one system-prompt channel opencode run has.
func opencodeSystemPrompt(env, argv []string, prompt string) ([]string, []string) {
	env, _ = vendors.MutateOpencodeConfigEnv(env, true, func(cfg map[string]any) {
		agents, _ := cfg["agent"].(map[string]any)
		if agents == nil {
			agents = map[string]any{}
		}
		agents[opencodeSystemAgent] = map[string]any{"mode": "primary", "prompt": prompt}
		cfg["agent"] = agents
	})
	if len(argv) > 0 && argv[0] == "run" {
		argv = append([]string{"run", "--agent", opencodeSystemAgent}, argv[1:]...)
	}
	return env, argv
}
