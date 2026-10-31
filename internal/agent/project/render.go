package project

import "strings"

// agentBanner is the first body line of every generated agent file; prune only ever touches files carrying it.
func agentBanner(agent Source, comment func(string) string) string {
	return comment("Synced from .claude/agents/" + agent.dirName + ".md; do not edit by hand.")
}

func htmlComment(s string) string { return "<!-- " + s + " -->" }
func hashComment(s string) string { return "# " + s }

// agentBody is the shared system prompt: memory section stripped, read-only note prepended when flagged.
func agentBody(agent Source) string {
	body := stripMemorySection(agent.body)
	if truthy(agent.frontmatter["readonly"]) {
		body = "**Note: this agent is read-only; do not modify files.**\n\n" + body
	}
	return strings.TrimRight(body, "\n") + "\n"
}

// renderGeminiAgent: ~/.gemini/agents/<name>.md — strict schema: name, description, and tools translated to
// Gemini's built-in names (an unknown name fails validation, so unmapped tools are dropped); body = system prompt.
func renderGeminiAgent(agent Source) string {
	fields := []orderedField{{"name", agent.Name()}, {"description", skillDescription(agent)}}
	if tools := geminiTools(toolList(agent.frontmatter["tools"])); len(tools) > 0 {
		fields = append(fields, orderedField{"tools", tools})
	}
	return buildFrontmatterFile(fields, agentBanner(agent, htmlComment)+"\n\n"+agentBody(agent))
}

// geminiToolNames maps Claude Code tool names onto Gemini CLI's built-in tool names (bundle *_TOOL_NAME constants).
var geminiToolNames = map[string]string{
	"Read": "read_file", "Write": "write_file", "Edit": "replace", "Bash": "run_shell_command",
	"Grep": "grep_search", "Glob": "glob", "LS": "list_directory", "WebFetch": "web_fetch", "WebSearch": "google_web_search",
}

// geminiTools translates a Claude tool list; names Gemini has no equivalent for (Agent, NotebookEdit, MCP) are dropped.
func geminiTools(claude []string) []string {
	var out []string
	for _, name := range claude {
		if g, ok := geminiToolNames[name]; ok {
			out = append(out, g)
		}
	}
	return out
}

// renderCopilotAgent: ~/.copilot/agents/<name>.md — Claude-style frontmatter loads as-is; tools pass through.
func renderCopilotAgent(agent Source) string {
	fields := []orderedField{{"name", agent.Name()}, {"description", skillDescription(agent)}}
	if tools := toolList(agent.frontmatter["tools"]); len(tools) > 0 {
		fields = append(fields, orderedField{"tools", tools})
	}
	return buildFrontmatterFile(fields, agentBanner(agent, htmlComment)+"\n\n"+agentBody(agent))
}

// renderOpenCodeAgent: ~/.config/opencode/agents/<name>.md — description + mode: subagent; body is the prompt.
func renderOpenCodeAgent(agent Source) string {
	fields := []orderedField{{"description", skillDescription(agent)}, {"mode", "subagent"}}
	return buildFrontmatterFile(fields, agentBanner(agent, htmlComment)+"\n\n"+agentBody(agent))
}

// renderCodexAgent: ~/.codex/agents/<name>.toml — name, description, developer_instructions, reasoning effort.
func renderCodexAgent(agent Source) string {
	var b strings.Builder
	b.WriteString(agentBanner(agent, hashComment))
	b.WriteString("\n")
	b.WriteString("name = ")
	b.WriteString(tomlBasic(agent.Name()))
	b.WriteString("\n")
	b.WriteString("description = ")
	b.WriteString(tomlMultiline(skillDescription(agent)))
	b.WriteString("\n")
	if effort := codexEffort(toString(agent.frontmatter["effort"])); effort != "" {
		b.WriteString("model_reasoning_effort = ")
		b.WriteString(tomlBasic(effort))
		b.WriteString("\n")
	}
	b.WriteString("developer_instructions = ")
	b.WriteString(tomlMultiline(agentBody(agent)))
	b.WriteString("\n")
	return b.String()
}

// codexEffort maps the harness effort scale onto codex's; "max" has no codex value and becomes xhigh.
func codexEffort(effort string) string {
	switch effort {
	case "low", "medium", "high", "xhigh":
		return effort
	case "max":
		return "xhigh"
	}
	return ""
}

// toolList normalizes `tools:` (comma string or YAML list) to a clean string slice.
func toolList(v any) []string {
	var raw []string
	switch t := v.(type) {
	case []string:
		raw = t
	case string:
		raw = strings.Split(t, ",")
	}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// tomlBasic renders a single-line TOML basic string.
func tomlBasic(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\r", `\r`)
	return `"` + r.Replace(s) + `"`
}

// tomlMultiline renders a TOML multi-line basic string; a `"""` inside the text is escaped as `""\"`.
func tomlMultiline(s string) string {
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"""`, `""\"`)
	return "\"\"\"\n" + escaped + "\"\"\""
}
