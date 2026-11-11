package project

import (
	"path/filepath"
	"sort"

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
}

// targets is every agent the sync projects agents into; skills need no per-agent table (see skills.go).
var targets = map[string]projectionTarget{
	agentid.Gemini:   {ext: ".md", home: sub(".gemini", "agents"), render: renderGeminiAgent, skills: geminiSkillsDir},
	agentid.Codex:    {ext: ".toml", home: sub(".codex", "agents"), render: renderCodexAgent},
	agentid.Copilot:  {ext: ".md", home: sub(".copilot", "agents"), render: renderCopilotAgent, skills: copilotSkillsDir},
	agentid.OpenCode: {ext: ".md", home: sub(".config", "opencode", "agents"), render: renderOpenCodeAgent},
}

// Names lists the sync targets, sorted.
func Names() []string {
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
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

// legacyGeminiSkillsDir is where earlier syncs projected skills; only banner-marked dirs there are ever pruned.
func legacyGeminiSkillsDir(home string) string { return filepath.Join(home, ".gemini", "skills") }

// geminiSkillsDir is agy's own skills dir: unlike other agents it does not read ~/.agents/skills, so
// the gemini agent pass links skills here too, in addition to the cross-agent agentsSkillsDir.
func geminiSkillsDir(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "skills")
}

// copilotSkillsDir is copilot's native skills dir; the copilot pass links skills here too, beside agentsSkillsDir.
func copilotSkillsDir(home string) string { return filepath.Join(home, ".copilot", "skills") }
