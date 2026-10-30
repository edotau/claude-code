package agent

import (
	"fmt"

	"github.com/edotau/claude-code/internal/providers"
)

// Adapter is the per-agent data behind the `claude-code <v> status|tail|result|cancel|setup|models` verbs.
type Adapter struct {
	Name           string                 // agent name == verb name: codex, gemini, copilot, opencode
	Binary         string                 // agent binary on PATH (gemini -> agy)
	Login          func() bool            // own CLI login presence
	ResumeHint     func(id string) string // the agent's own resume command, for display only
	ModelsArgv     []string               // agent args that list models; nil = no agent catalog verb
	ModelsProvider string                 // non-empty = list via claude-code models --live on this provider's login profile, not the agent
	ReadNeedsTools bool                   // true when the agent denies file reads/search too unless --tools is set
	ExtraUsage     [][2]string            // extra help-page command rows (label, description) beyond the common roster
}

// adapters is the roster, in verb order.
var adapters = []Adapter{
	{
		Name:           "codex",
		Login:          CodexChatGPTLoginPresent,
		ResumeHint:     func(id string) string { return "codex resume " + id },
		ModelsProvider: "openai",
		ReadNeedsTools: false, // codex exec's --sandbox read-only still reads files without --tools
	},
	{
		Name:           "gemini",
		Login:          GeminiLoginPresent,
		ResumeHint:     func(id string) string { return "agy --conversation " + id },
		ModelsArgv:     []string{"models"},
		ReadNeedsTools: true, // agy's no-tools preamble bars @path reads; needs --tools to read/search
		ExtraUsage:     [][2]string{{"bridge", "ingest files for a large-context task"}},
	},
	{
		Name:           "copilot",
		Login:          CopilotLoginPresent,
		ResumeHint:     func(id string) string { return "copilot --resume " + id },
		ModelsProvider: "github",
		ReadNeedsTools: true, // denyAllTools blocks every tool call, including file reads, unless AllowAllTools
	},
	{
		Name:           "opencode",
		Login:          func() bool { return len(OpencodeLogins()) > 0 },
		ResumeHint:     func(id string) string { return "opencode run --session " + id },
		ModelsArgv:     []string{"models"},
		ReadNeedsTools: true, // permission:"deny" without --tools blocks every tool, including read/glob
	},
}

// LookupAdapter returns the adapter by name, Binary filled from the agents registry.
func LookupAdapter(name string) (Adapter, bool) {
	for _, a := range adapters {
		if a.Name != name {
			continue
		}
		a.Binary = AgentBinary(name)
		return a, true
	}
	return Adapter{}, false
}

// errUnknownAgent is the shared "no adapter" error, so callers report consistently.
func errUnknownAgent(name string) error { return fmt.Errorf("unknown agent %q", name) }

// ResumeHint renders Adapter.ResumeHint for id, empty when the vendor has none.
func ResumeHint(vendor, id string) string {
	a, ok := LookupAdapter(vendor)
	if !ok || a.ResumeHint == nil || id == "" {
		return ""
	}
	return a.ResumeHint(id)
}

// ModelsProfile is the vendor's profile on its ModelsProvider that carries the vendor's own login.
func ModelsProfile(reg *providers.Registry, vendor string) (string, error) {
	a, _ := LookupAdapter(vendor)
	for _, n := range reg.ProfilesOf(a.ModelsProvider) {
		if reg.Providers[n].Auth.Login == vendor {
			return n, nil
		}
	}
	return "", fmt.Errorf("%s: no %s profile with a %s login", vendor, a.ModelsProvider, vendor)
}
