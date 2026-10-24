package crossgen

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/frontmatter"
)

// hasGeneratedBanner only recognises the banner as the FIRST non-empty body line; a hand-authored file that
// merely quotes the banner text further down must read as hand-authored (KEEP).
func TestGeneratedBannerMustBeFirstBodyLine(t *testing.T) {
	dir := t.TempDir()

	hand := filepath.Join(dir, "x.md")
	writeFile(t, hand, "---\nname: x\ndescription: d\n---\n\n"+
		"Some hand-written prose.\n\nIt later quotes: "+agentSkillBannerPrefix+"x.md; do not edit by hand. -->\n")
	if isGeneratedFile(hand) {
		t.Error("banner quoted later in the body must not be recognised as generated (would KEEP)")
	}

	gen := filepath.Join(dir, "y.md")
	writeFile(t, gen, "---\nname: y\ndescription: d\n---\n\n"+agentSkillBannerPrefix+"y.md; do not edit by hand. -->\n\nBody.\n")
	if !isGeneratedFile(gen) {
		t.Error("banner as the first body line must be recognised as generated")
	}

	toml := filepath.Join(dir, "z.toml")
	writeFile(t, toml, codexBannerPrefix+"z.md; do not edit by hand.\nname = \"z\"\n")
	if !isGeneratedFile(toml) {
		t.Error("codex hash banner on line one must be recognised as generated")
	}

	legacy := filepath.Join(dir, "skill")
	writeFile(t, filepath.Join(legacy, "SKILL.md"), "---\nname: s\n---\n\n"+realSkillBannerPrefix+"s/SKILL.md; do not edit by hand. -->\n")
	if !isGeneratedSkillDir(legacy) {
		t.Error("legacy skill projection dir must still be recognised for the ~/.gemini/skills prune")
	}
}

// Every renderer must stamp the banner as the first body line, or its own output could never be pruned.
func TestEveryRendererStampsPrunableBanner(t *testing.T) {
	agent := Source{dirName: "demo", frontmatter: map[string]any{"name": "demo", "description": "Does things. <example>x</example>", "tools": "Read, Grep", "effort": "max"}, body: "# demo\n\n## Memory\nkeep out\n\n## Work\nbody\n"}
	for _, name := range VendorNames() {
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
	fm, body := frontmatter.Parse(renderGeminiAgent(agent))
	if len(fm) != 3 || fm["name"] != "demo" || fm["description"] != "d" {
		t.Errorf("gemini frontmatter = %v, want exactly name + description + tools", fm)
	}
	if tools, _ := fm["tools"].([]string); strings.Join(tools, ",") != "read_file,run_shell_command" {
		t.Errorf("gemini tools = %v, want [read_file run_shell_command] (Agent dropped)", fm["tools"])
	}
	fm, _ = frontmatter.Parse(renderGeminiAgent(Source{dirName: "x", frontmatter: map[string]any{"name": "x", "description": "d", "tools": "Agent"}}))
	if _, has := fm["tools"]; has {
		t.Errorf("all-unmappable tools must omit the key, got %v", fm["tools"])
	}
	if !strings.HasPrefix(body, agentSkillBannerPrefix) {
		t.Errorf("gemini body must open with the banner: %q", body)
	}
}

// Copilot loads Claude-style frontmatter, so tools pass through as a list; opencode needs mode: subagent.
func TestRenderCopilotAndOpenCodeFrontmatter(t *testing.T) {
	agent := Source{dirName: "demo", frontmatter: map[string]any{"name": "demo", "description": "d", "tools": "Read, Grep"}, body: "body\n"}
	fm, _ := frontmatter.Parse(renderCopilotAgent(agent))
	if tools, _ := fm["tools"].([]string); strings.Join(tools, ",") != "Read,Grep" {
		t.Errorf("copilot tools = %v, want [Read Grep]", fm["tools"])
	}
	fm, _ = frontmatter.Parse(renderOpenCodeAgent(agent))
	if fm["mode"] != "subagent" || fm["description"] != "d" {
		t.Errorf("opencode frontmatter = %v, want description + mode: subagent", fm)
	}
}
