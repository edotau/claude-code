package project

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

// hasGeneratedBanner only recognises the banner as the FIRST non-empty body line; a hand-authored file that
// merely quotes the banner text further down must read as hand-authored (KEEP).
func TestGeneratedBannerMustBeFirstBodyLine(t *testing.T) {
	dir := t.TempDir()

	hand := filepath.Join(dir, "x.md")
	projectWriteFile(t, hand, "---\nname: x\ndescription: d\n---\n\n"+
		"Some hand-written prose.\n\nIt later quotes: "+agentSkillBannerPrefix+"x.md; do not edit by hand. -->\n")
	if isGeneratedFile(hand) {
		t.Error("banner quoted later in the body must not be recognised as generated (would KEEP)")
	}

	gen := filepath.Join(dir, "y.md")
	projectWriteFile(t, gen, "---\nname: y\ndescription: d\n---\n\n"+agentSkillBannerPrefix+"y.md; do not edit by hand. -->\n\nBody.\n")
	if !isGeneratedFile(gen) {
		t.Error("banner as the first body line must be recognised as generated")
	}

	toml := filepath.Join(dir, "z.toml")
	projectWriteFile(t, toml, codexBannerPrefix+"z.md; do not edit by hand.\nname = \"z\"\n")
	if !isGeneratedFile(toml) {
		t.Error("codex hash banner on line one must be recognised as generated")
	}

	legacy := filepath.Join(dir, "skill")
	projectWriteFile(t, filepath.Join(legacy, "SKILL.md"), "---\nname: s\n---\n\n"+realSkillBannerPrefix+"s/SKILL.md; do not edit by hand. -->\n")
	if !isGeneratedSkillDir(legacy) {
		t.Error("legacy skill projection dir must still be recognised for the ~/.gemini/skills prune")
	}
}

// Every renderer must stamp the banner as the first body line, or its own output could never be pruned.
func TestEveryRendererStampsPrunableBanner(t *testing.T) {
	agent := Source{dirName: "demo", frontmatter: map[string]any{"name": "demo", "description": "Does things. <example>x</example>", "tools": "Read, Grep", "effort": "max"}, body: "# demo\n\n## Memory\nkeep out\n\n## Work\nbody\n"}
	for _, name := range Names() {
		content := targets[name].render(agent)
		if !hasGeneratedBanner(content) {
			t.Errorf("%s: rendered agent lacks a prunable banner:\n%s", name, content)
		}
		if strings.Contains(content, "<example>") || strings.Contains(content, "keep out") {
			t.Errorf("%s: examples or the Memory section leaked into the projection:\n%s", name, content)
		}
	}
}

// Codex TOML: effort maps onto codex's scale, and the body travels as a multi-line basic string with `"""` escaped.
func TestRenderCodexAgent(t *testing.T) {
	agent := Source{dirName: "demo", frontmatter: map[string]any{"name": "demo", "description": "Quote \"this\"", "effort": "max"}, body: "say \"\"\"hi\"\"\" and C:\\path\n"}
	got := renderCodexAgent(agent)
	for _, want := range []string{
		codexBannerPrefix + "demo.md; do not edit by hand.\n",
		"name = \"demo\"\n",
		"model_reasoning_effort = \"xhigh\"\n",
		"developer_instructions = \"\"\"\nsay \"\"\\\"hi\"\"\\\" and C:\\\\path\n\"\"\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("codex render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(renderCodexAgent(Source{dirName: "d", frontmatter: map[string]any{"name": "d"}}), "model_reasoning_effort") {
		t.Error("no effort: key must not emit model_reasoning_effort")
	}
}

// Gemini's agent schema is strict: name, description and translated tools only — never tier words, colors, or a
// Claude tool name Gemini would reject; an agent with only unmappable tools omits the key (inherit all).
func TestRenderGeminiAgentEmitsOnlyStrictFields(t *testing.T) {
	agent := Source{dirName: "demo", frontmatter: map[string]any{"name": "demo", "description": "d", "tools": "Read, Agent, Bash", "model": "sonnet", "color": "cyan"}, body: "body\n"}
	fm, body := textutil.ParseFrontmatter(renderGeminiAgent(agent))
	if len(fm) != 3 || fm["name"] != "demo" || fm["description"] != "d" {
		t.Errorf("gemini frontmatter = %v, want exactly name + description + tools", fm)
	}
	if tools, _ := fm["tools"].([]string); strings.Join(tools, ",") != "read_file,run_shell_command" {
		t.Errorf("gemini tools = %v, want [read_file run_shell_command] (Agent dropped)", fm["tools"])
	}
	fm, _ = textutil.ParseFrontmatter(renderGeminiAgent(Source{dirName: "x", frontmatter: map[string]any{"name": "x", "description": "d", "tools": "Agent"}}))
	if _, has := fm["tools"]; has {
		t.Errorf("all-unmappable tools must omit the key, got %v", fm["tools"])
	}
	if !strings.HasPrefix(body, agentSkillBannerPrefix) {
		t.Errorf("gemini body must open with the banner: %q", body)
	}
}

// Copilot gets tools in its own names with untranslatable ones listed in one comment line; model only when concrete.
func TestRenderCopilotAgentTranslatesTools(t *testing.T) {
	agent := Source{dirName: "demo", frontmatter: map[string]any{"name": "demo", "description": "d", "tools": "Read, Grep, Glob, Bash, mcp__x__y, NotebookEdit", "model": "sonnet"}, body: "body\n"}
	got := renderCopilotAgent(agent)
	fm, body := textutil.ParseFrontmatter(got)
	if tools, _ := fm["tools"].([]string); strings.Join(tools, ",") != "read,search,execute" {
		t.Errorf("copilot tools = %v, want [read search execute]", fm["tools"])
	}
	if _, has := fm["model"]; has {
		t.Errorf("tier alias must not map to a copilot model: %v", fm["model"])
	}
	lines := strings.Split(body, "\n")
	if !strings.HasPrefix(lines[0], agentSkillBannerPrefix) || lines[2] != "<!-- Tools with no Copilot equivalent, dropped: mcp__x__y, NotebookEdit -->" {
		t.Errorf("want banner then one dropped-tools comment, got:\n%s", body)
	}
	agent.frontmatter["model"] = "gpt-5"
	agent.frontmatter["tools"] = "Read"
	got = renderCopilotAgent(agent)
	fm, body = textutil.ParseFrontmatter(got)
	if fm["model"] != "gpt-5" || strings.Contains(body, "no Copilot equivalent") {
		t.Errorf("concrete model kept and no comment when nothing dropped, got:\n%s", got)
	}
}

// opencode: read-only tool lists get permission edit/bash deny; writers and inherit-all agents do not.
func TestRenderOpenCodeAgentPermissions(t *testing.T) {
	mk := func(tools any) Source {
		fm := map[string]any{"name": "demo", "description": "d"}
		if tools != nil {
			fm["tools"] = tools
		}
		return Source{dirName: "demo", frontmatter: fm, body: "body\n"}
	}
	golden := "---\ndescription: \"d\"\nmode: \"subagent\"\npermission:\n  edit: deny\n  bash: deny\n---\n\n" + htmlComment("Synced from .claude/agents/demo.md; do not edit by hand.") + "\n\nbody\n"
	if got := renderOpenCodeAgent(mk("Read, Grep, Glob")); got != golden {
		t.Errorf("read-only render =\n%q\nwant\n%q", got, golden)
	}
	for _, tools := range []any{"Read, Bash", "Read, Edit", "Write", nil} {
		if got := renderOpenCodeAgent(mk(tools)); strings.Contains(got, "permission") {
			t.Errorf("tools %v must not be denied:\n%s", tools, got)
		}
	}
	fm, _ := textutil.ParseFrontmatter(renderOpenCodeAgent(mk("Read")))
	if fm["mode"] != "subagent" || fm["description"] != "d" {
		t.Errorf("opencode frontmatter = %v, want description + mode: subagent", fm)
	}
}
