package ask

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/assets"
)

// explorePrompt builds the Codebase Context Brief prompt for question; web adds the web-research dispatch.
func explorePrompt(question string, web bool) string { return assets.ExplorePrompt(question, web) }

// takeoverBrief is the fixed Pi takeover brief: agent, cutoff, the original task, the partial output.
func takeoverBrief(agentName, cut, task string, n int, partial string) string {
	return assets.TakeoverBrief(agentName, cut, task, n, partial)
}

// needsTools reports whether the agent must run with tools on to read the repo.
func needsTools(agentName string) bool {
	s, ok := agent.LookupSchema(agentName)
	return ok && s.CLI.ReadNeedsTools
}

// ExploreArgs builds the `ask` argv for an explore run: --tools only when the agent needs it to read, else tools off.
func ExploreArgs(agentName, cwd, question string, web, background bool) ([]string, error) {
	args := []string{"--agent", agentName, "--workdir", cwd, "--tools=false"}
	if needsTools(agentName) {
		args[len(args)-1] = "--tools"
	}
	if background {
		args = append(args, "--background")
	}
	return append(args, explorePrompt(question, web)), nil
}
