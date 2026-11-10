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
	tools := geminiTools(toolList(agent.frontmatter["tools"]))
	fields := []orderedField{{"name", agent.Name()}, {"description", skillDescription(agent)}}
	if len(tools) > 0 {
		fields = append(fields, orderedField{"tools", tools})
	}
	return renderMarkdownAgent(agent, fields)
}

// renderMarkdownAgent is the frontmatter + banner + body tail every markdown agent renderer ends with.
func renderMarkdownAgent(agent Source, fields []orderedField) string {
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

// copilotToolNames maps Claude Code tool names onto Copilot's custom-agent tool aliases.
var copilotToolNames = map[string]string{
	"Read": "read", "Grep": "search", "Glob": "search", "LS": "search", "Edit": "edit", "Write": "edit",
	"Bash": "execute", "Agent": "agent", "WebFetch": "web", "WebSearch": "web",
}

// copilotTools translates a Claude tool list (deduplicated, order kept) and returns the names with no Copilot alias.
func copilotTools(claude []string) (mapped, dropped []string) {
	seen := map[string]bool{}
	for _, name := range claude {
		c, ok := copilotToolNames[name]
		switch {
		case !ok:
			dropped = append(dropped, name)
		case !seen[c]:
			seen[c] = true
			mapped = append(mapped, c)
		}
	}
	return mapped, dropped
}

// concreteModel returns the frontmatter model when it names a real model; Claude tier aliases have no other-CLI route.
func concreteModel(agent Source) string {
	m := toString(agent.frontmatter["model"])
	switch strings.ToLower(m) {
	case "", "inherit", "sonnet", "opus", "haiku":
		return ""
	}
	return m
}

// readOnlyTools reports an explicit tools: list that grants none of Edit, Write or Bash.
func readOnlyTools(agent Source) bool {
	tools := toolList(agent.frontmatter["tools"])
	for _, t := range tools {
		if t == "Edit" || t == "Write" || t == "Bash" {
			return false
		}
	}
	return len(tools) > 0
}

// renderCopilotAgent: ~/.copilot/agents/<name>.md — tools in Copilot's names (untranslatable ones listed in a
// comment line under the banner), model only when concrete.
func renderCopilotAgent(agent Source) string {
	tools, dropped := copilotTools(toolList(agent.frontmatter["tools"]))
	fields := []orderedField{{"name", agent.Name()}, {"description", skillDescription(agent)}}
	if len(tools) > 0 {
		fields = append(fields, orderedField{"tools", tools})
	}
	if m := concreteModel(agent); m != "" {
		fields = append(fields, orderedField{"model", m})
	}
	note := ""
	if len(dropped) > 0 {
		note = htmlComment("Tools with no Copilot equivalent, dropped: "+strings.Join(dropped, ", ")) + "\n\n"
	}
	return buildFrontmatterFile(fields, agentBanner(agent, htmlComment)+"\n\n"+note+agentBody(agent))
}

// renderOpenCodeAgent: ~/.config/opencode/agents/<name>.md — description + mode: subagent; read-only agents deny
// edit and bash; model only when concrete.
func renderOpenCodeAgent(agent Source) string {
	fields := []orderedField{{"description", skillDescription(agent)}, {"mode", "subagent"}}
	if m := concreteModel(agent); m != "" {
		fields = append(fields, orderedField{"model", m})
	}
	out := renderMarkdownAgent(agent, fields)
	if !readOnlyTools(agent) {
		return out
	}
	// The field writer only emits scalars and flat lists, so the nested permission block is spliced in before the closing ---.
	return strings.Replace(out, "\n---\n\n", "\npermission:\n  edit: deny\n  bash: deny\n---\n\n", 1)
}

// renderCodexAgent: ~/.codex/agents/<name>.toml — name, description, developer_instructions, reasoning effort.
func renderCodexAgent(agent Source) string {
	lines := []string{
		agentBanner(agent, hashComment),
		"name = " + tomlBasic(agent.Name()),
		"description = " + tomlMultiline(skillDescription(agent)),
	}
	if effort := codexEffort(toString(agent.frontmatter["effort"])); effort != "" {
		lines = append(lines, "model_reasoning_effort = "+tomlBasic(effort))
	}
	lines = append(lines, "developer_instructions = "+tomlMultiline(agentBody(agent)))
	return strings.Join(lines, "\n") + "\n"
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
