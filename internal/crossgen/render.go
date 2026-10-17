package crossgen

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

// renderGeminiAgent: ~/.gemini/agents/<name>.md — strict schema, so only name + description are emitted
// (Claude tool names and tier words would fail validation); the body is the system prompt.
func renderGeminiAgent(agent Source) string {
	fields := []orderedField{{"name", agent.Name()}, {"description", skillDescription(agent)}}
	return buildFrontmatterFile(fields, agentBanner(agent, htmlComment)+"\n\n"+agentBody(agent))
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
	b.WriteString(agentBanner(agent, hashComment) + "\n")
	b.WriteString("name = " + tomlBasic(agent.Name()) + "\n")
	b.WriteString("description = " + tomlMultiline(skillDescription(agent)) + "\n")
	if effort := codexEffort(toString(agent.frontmatter["effort"])); effort != "" {
		b.WriteString("model_reasoning_effort = " + tomlBasic(effort) + "\n")
	}
	b.WriteString("developer_instructions = " + tomlMultiline(agentBody(agent)) + "\n")
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
