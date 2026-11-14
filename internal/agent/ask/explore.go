package ask

import (
	"fmt"

	"github.com/edotau/claude-code/internal/agent"
)

// explorePromptBase is the read-only Explore brief shared by every agent; the web part is appended when enabled.
const explorePromptBase = `Read-only: never modify, create or delete a file.
Orient first: list the directories that matter, read the README/spec for the area, then read narrowly.
Cite file:line for every claim about the code.`

// explorePromptWeb appends the web-research dispatch, cited by URL.
const explorePromptWeb = `
Then research the relevant libraries, APIs and prior art on the web, citing a URL for every claim.
If a source can't be reached, say so instead of guessing.`

// explorePromptTail fixes the five required sections and the stopping rule.
const explorePromptTail = `
Output exactly these five sections, each a few bullets: Structure, Conventions, Dependencies, Prior art,
Open questions. Stop once two more lookups wouldn't change the brief.

Question: `

// explorePrompt builds the Codebase Context Brief prompt for question; web adds the web-research dispatch.
func explorePrompt(question string, web bool) string {
	p := explorePromptBase
	if web {
		p += explorePromptWeb
	}
	return p + explorePromptTail + question
}

// needsTools reports whether the agent must run with tools on to read the repo.
func needsTools(agentName string) bool {
	s, ok := agent.LookupSchema(agentName)
	return ok && s.CLI.ReadNeedsTools
}

// enforcesReadOnly reports whether the agent's own CLI enforces --read-only (not every vendor can).
func enforcesReadOnly(agentName string) bool {
	s, ok := agent.LookupSchema(agentName)
	return ok && s.CLI.Caps.ReadOnly == agent.Native
}

// requireReadOnly refuses an agent that reads with tools but cannot enforce read-only (prompt-only is not enough).
func requireReadOnly(agentName string) error {
	if !needsTools(agentName) || enforcesReadOnly(agentName) {
		return nil
	}
	return fmt.Errorf("explore on %s needs native read-only support, which it emulates with permission denies; use codex or copilot", agentName)
}

// ExploreArgs builds the `ask` argv for an explore run: --tools only when the agent needs it to read.
func ExploreArgs(agentName, cwd, question string, web, background bool) ([]string, error) {
	if err := requireReadOnly(agentName); err != nil {
		return nil, err
	}
	args := []string{"--agent", agentName, "--workdir", cwd}
	if needsTools(agentName) {
		args = append(args, "--tools", "--read-only")
	}
	if background {
		args = append(args, "--background")
	}
	return append(args, explorePrompt(question, web)), nil
}
