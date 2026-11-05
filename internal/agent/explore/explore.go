// Package explore builds read-only Explore runs: the brief prompt, the ask argv, per-leg options and the split.
package explore

import "github.com/edotau/claude-code/internal/agent"

// promptBase is the read-only Explore brief shared by every agent; web is appended when enabled.
const promptBase = `Read-only: never modify, create or delete a file.
Orient first: list the directories that matter, read the README/spec for the area, then read narrowly.
Cite file:line for every claim about the code.`

// promptWeb appends the web-research dispatch, cited by URL.
const promptWeb = `
Then research the relevant libraries, APIs and prior art on the web, citing a URL for every claim.
If a source can't be reached, say so instead of guessing.`

// promptTail fixes the five required sections and the stopping rule.
const promptTail = `
Output exactly these five sections, each a few bullets: Structure, Conventions, Dependencies, Prior art,
Open questions. Stop once two more lookups wouldn't change the brief.

Question: `

// Prompt builds the Codebase Context Brief prompt for question; web adds the web-research dispatch.
func Prompt(question string, web bool) string {
	p := promptBase
	if web {
		p += promptWeb
	}
	return p + promptTail + question
}

// needsTools reports whether the agent must run with tools on to read the repo.
func needsTools(agentName string) bool {
	s, ok := agent.LookupSchema(agentName)
	return ok && s.CLI.ReadNeedsTools
}

// Args builds the `ask` argv for an explore run: --tools only when the agent needs it to read.
func Args(agentName, cwd, question string, web, background bool) []string {
	args := []string{"--agent", agentName, "--workdir", cwd}
	if needsTools(agentName) {
		args = append(args, "--tools")
	}
	if background {
		args = append(args, "--background")
	}
	return append(args, Prompt(question, web))
}

// Options returns base with the explore prompt for question, and tools on when the agent needs them to read.
func Options(base agent.RunOptions, question string, web bool) agent.RunOptions {
	o := base
	o.Prompt = Prompt(question, web)
	if needsTools(o.Agent) {
		o.Tools = true
	}
	return o
}
