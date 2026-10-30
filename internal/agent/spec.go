package agent

import (
	"github.com/edotau/claude-code/internal/providers"
)

// Spec is an agent's row (providers.AgentSpec): CLI binary, wires, router paths and pin keys.
type Spec = providers.AgentSpec

// Names lists the third-party agents in roster order.
func Names() []string {
	var out []string
	for _, s := range providers.AgentSpecs() {
		if s.Binary != "" {
			out = append(out, s.Name)
		}
	}
	return out
}

// LookupSpec returns the row of a third-party agent by name (claude has none).
func LookupSpec(name string) (Spec, bool) {
	s, ok := providers.LookupAgentSpec(name)
	return s, ok && s.Binary != ""
}

// Headless is the non-interactive form of a run; zero Tools means tools are disabled where the CLI allows it.
type Headless struct {
	Prompt     string
	Effort     string
	SessionID  string // resume this session
	Tools      bool
	Autonomous bool     // opencode: --auto
	Attach     []string // opencode: one -f per path
	WorkDir    string   // opencode: --dir; cmd.Dir alone doesn't move opencode's own cwd resolution
	NoPersist  bool     // codex: --ephemeral
	SafeMode   bool     // codex: --ignore-user-config --ignore-rules; opencode: --pure
}
