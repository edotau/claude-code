package agent

import (
	"encoding/json"
	"strings"
)

// opencodeSystemAgent is the custom agent whose prompt carries --system-prompt for opencode.
const opencodeSystemAgent = "harness-system"

// opencodeSystemPrompt defines a primary agent with prompt in OPENCODE_CONFIG_CONTENT (merged into any config
// already there) and selects it with --agent, the one system-prompt channel opencode run has.
func opencodeSystemPrompt(env, argv []string, prompt string) ([]string, []string) {
	const key = "OPENCODE_CONFIG_CONTENT="
	cfg := map[string]any{}
	at := -1
	for i, kv := range env {
		if body, ok := strings.CutPrefix(kv, key); ok {
			at = i
			_ = json.Unmarshal([]byte(body), &cfg) // an unreadable block is replaced, not extended
		}
	}
	agents, _ := cfg["agent"].(map[string]any)
	if agents == nil {
		agents = map[string]any{}
	}
	agents[opencodeSystemAgent] = map[string]any{"mode": "primary", "prompt": prompt}
	cfg["agent"] = agents
	body, _ := json.Marshal(cfg)
	if at >= 0 {
		env[at] = key + string(body)
	} else {
		env = append(env, key+string(body))
	}
	if len(argv) > 0 && argv[0] == "run" {
		argv = append([]string{"run", "--agent", opencodeSystemAgent}, argv[1:]...)
	}
	return env, argv
}
