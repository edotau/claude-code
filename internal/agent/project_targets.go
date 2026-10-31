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
	home   func(home string) string  // vendor agents dir under $HOME
	render func(agent Source) string // full file content, banner included
	skills func(home string) string  // the CLI's own skills dir when it does not read ~/.agents/skills; nil = shared
}

// targets is every vendor the sync projects agents into; skills need no per-vendor table (see skills.go).
var targets = map[string]projectionTarget{
	"gemini":   {name: "gemini", ext: ".md", home: sub(".gemini", "agents"), render: renderGeminiAgent, skills: GeminiSkillsDir},
	"codex":    {name: "codex", ext: ".toml", home: sub(".codex", "agents"), render: renderCodexAgent},
	"copilot":  {name: "copilot", ext: ".md", home: sub(".copilot", "agents"), render: renderCopilotAgent},
	"opencode": {name: "opencode", ext: ".md", home: sub(".config", "opencode", "agents"), render: renderOpenCodeAgent},
}

// VendorNames lists the sync targets, sorted.
func VendorNames() []string {
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

// ProjectionRoot is a vendor's local rendering under harness state (gitignored, rebuilt by every sync).
func ProjectionRoot(root, vendor string) string {
	return filepath.Join(root, "state", "projections", vendor)
}

// ProjectionDir holds a vendor's rendered agents: <config>/state/projections/<vendor>/agents/<name>.<ext>.
func ProjectionDir(root, vendor string) string {
	return filepath.Join(ProjectionRoot(root, vendor), "agents")
}

// AgentsSkillsDir is the cross-vendor skills dir every CLI reads natively: $HOME/.agents/skills.
func AgentsSkillsDir(home string) string { return filepath.Join(home, ".agents", "skills") }

// LegacyGeminiSkillsDir is where earlier syncs projected skills; only banner-marked dirs there are ever pruned.
func LegacyGeminiSkillsDir(home string) string { return filepath.Join(home, ".gemini", "skills") }

// GeminiSkillsDir is agy's own skills dir: unlike other vendors it does not read ~/.agents/skills, so
// the gemini vendor pass links skills here too, in addition to the cross-vendor AgentsSkillsDir.
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
