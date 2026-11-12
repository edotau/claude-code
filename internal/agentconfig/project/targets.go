package project

import (
	"maps"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

var _ = schemas.Register(schemas.Format{
	Name: "projections", Owner: "internal/agentconfig/project", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.ConfigDir(), "state", "projections", "*") },
	Version: 1, Clean: "projections",
})

// projectionTarget is one agent CLI's native subagent surface: where it reads agents and how one is rendered.
type projectionTarget struct {
	ext    string                    // output extension incl. dot
	home   func(home string) string  // agent agents dir under $HOME
	render func(agent Source) string // full file content, banner included
	skills func(home string) string  // the CLI's own skills dir when it does not read ~/.agents/skills; nil = shared
	docs   string                    // global instruction file relative to $HOME; "" = the CLI reads none
}

// targets is every agent the sync projects agents into; skills need no per-agent table (see skills.go).
var targets = map[string]projectionTarget{
	agentid.Gemini: {ext: ".md", home: sub(".gemini", "agents"), render: renderGeminiAgent, skills: geminiSkillsDir, docs: filepath.Join(".gemini", "GEMINI.md")},
	agentid.Codex:  {ext: ".toml", home: sub(".codex", "agents"), render: renderCodexAgent, docs: filepath.Join(".codex", "AGENTS.md")},
	agentid.Copilot: {ext: ".md", home: sub(".copilot", "agents"), render: renderCopilotAgent, skills: copilotSkillsDir,
		docs: filepath.Join(".copilot", "copilot-instructions.md")},
	agentid.OpenCode: {ext: ".md", home: sub(".config", "opencode", "agents"), render: renderOpenCodeAgent},
}

// Names lists the sync targets, sorted.
func Names() []string {
	return slices.Sorted(maps.Keys(targets))
}

func sub(parts ...string) func(string) string {
	return func(home string) string { return filepath.Join(append([]string{home}, parts...)...) }
}

// Root is an agent's local rendering under harness state (gitignored, rebuilt by every sync).
func Root(root, name string) string {
	return filepath.Join(root, "state", "projections", name)
}

// Dir holds an agent's rendered agents: <config>/state/projections/<agent>/agents/<name>.<ext>.
func Dir(root, name string) string {
	return filepath.Join(Root(root, name), "agents")
}

// agentsSkillsDir is the cross-agent skills dir every CLI reads natively: $HOME/.agents/skills.
func agentsSkillsDir(home string) string { return filepath.Join(home, ".agents", "skills") }

// geminiSkillsDir is agy's own skills dir (it skips ~/.agents/skills), so the gemini pass links skills here too.
func geminiSkillsDir(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "skills")
}

// copilotSkillsDir is copilot's native skills dir; the copilot pass links skills here too, beside agentsSkillsDir.
func copilotSkillsDir(home string) string { return filepath.Join(home, ".copilot", "skills") }
