package agent

import (
	"os"
	"path/filepath"
	"sort"
)

// projectionTarget is one agent CLI's native subagent surface: where it reads agents and how one is rendered.
type projectionTarget struct {
	name   string
	ext    string                    // output extension incl. dot
	home   func(home string) string  // agent agents dir under $HOME
	render func(agent Source) string // full file content, banner included
	skills func(home string) string  // the CLI's own skills dir when it does not read ~/.agents/skills; nil = shared
}

// targets is every agent the sync projects agents into; skills need no per-agent table (see skills.go).
var targets = map[string]projectionTarget{
	"gemini":   {name: "gemini", ext: ".md", home: sub(".gemini", "agents"), render: renderGeminiAgent, skills: GeminiSkillsDir},
	"codex":    {name: "codex", ext: ".toml", home: sub(".codex", "agents"), render: renderCodexAgent},
	"copilot":  {name: "copilot", ext: ".md", home: sub(".copilot", "agents"), render: renderCopilotAgent},
	"opencode": {name: "opencode", ext: ".md", home: sub(".config", "opencode", "agents"), render: renderOpenCodeAgent},
}

// ProjectionNames lists the sync targets, sorted.
func ProjectionNames() []string {
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

// ProjectionRoot is an agent's local rendering under harness state (gitignored, rebuilt by every sync).
func ProjectionRoot(root, name string) string {
	return filepath.Join(root, "state", "projections", name)
}

// ProjectionDir holds an agent's rendered agents: <config>/state/projections/<agent>/agents/<name>.<ext>.
func ProjectionDir(root, name string) string {
	return filepath.Join(ProjectionRoot(root, name), "agents")
}

// AgentsSkillsDir is the cross-agent skills dir every CLI reads natively: $HOME/.agents/skills.
func AgentsSkillsDir(home string) string { return filepath.Join(home, ".agents", "skills") }

// LegacyGeminiSkillsDir is where earlier syncs projected skills; only banner-marked dirs there are ever pruned.
func LegacyGeminiSkillsDir(home string) string { return filepath.Join(home, ".gemini", "skills") }

// GeminiSkillsDir is agy's own skills dir: unlike other agents it does not read ~/.agents/skills, so
// the gemini agent pass links skills here too, in addition to the cross-agent AgentsSkillsDir.
func GeminiSkillsDir(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "skills")
}

func userHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
