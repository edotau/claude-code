package agent

// explorePromptBase is the read-only Explore brief shared by every agent; web is appended when enabled.
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

// ExplorePrompt builds the Codebase Context Brief prompt for question; web adds the web-research dispatch.
func ExplorePrompt(question string, web bool) string {
	p := explorePromptBase
	if web {
		p += explorePromptWeb
	}
	return p + explorePromptTail + question
}

// ExploreArgs builds the `ask` argv for an explore run: --tools only when the agent needs it to read.
func ExploreArgs(agentName, cwd, question string, web, background bool) []string {
	args := []string{"--agent", agentName, "--workdir", cwd}
	if s, ok := LookupSchema(agentName); ok && s.CLI.ReadNeedsTools {
		args = append(args, "--tools")
	}
	if background {
		args = append(args, "--background")
	}
	return append(args, ExplorePrompt(question, web))
}
