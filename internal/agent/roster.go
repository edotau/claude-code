package agent

import (
	"fmt"
	"maps"
	"slices"

	"github.com/edotau/claude-code/internal/providers"
)

// Schema is one agent definition: a roster CLI, or a prompt-bearing definition that runs on one.
type Schema struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools"` // nil = inherit, empty = no tools
	Model       string   `json:"model,omitempty"`
	MaxTurns    int      `json:"max_turns,omitempty"`
	Runner      string   `json:"runner,omitempty"`  // roster agent that runs it; "" = claude
	Profile     string   `json:"profile,omitempty"` // provider profile; "" = routed
	Effort      string   `json:"effort,omitempty"`
	CLI         CLI      `json:"-"` // roster facts; zero on a definition
}

// CLI is a roster row's platform group.
type CLI struct {
	providers.AgentSpec // Name, Binary, Dialects, RouterSuffix, pin keys, version bounds
	Caps                Caps
	Events              []string    // typed event kinds the stream can carry
	Cost                bool        // reports a cost
	ResumeFormat        string      // "codex resume %s"; "" = none
	ModelsArgv          []string    // args that list models; nil = no catalog verb
	ModelsProvider      string      // list via models --live on this provider instead
	ReadNeedsTools      bool        // file reads are denied without --tools
	PromptInArgv        bool        // prompt rides argv, so MaxPromptBytes applies
	DirFlag             bool        // Headless.WorkDir must be set; cmd.Dir alone is not enough
	ConfigureHint       string      // the command to run when no credential resolved
	DisplayName         string      // TUI label; "" = Name
	LoginArgv           []string    // the CLI's own sign-in command
	ExtraUsage          [][2]string // extra help rows
	CatalogUsage        string      // override for the catalog help row
	CatalogHint         string      // extra hint on the catalog page
}

var rosterRows = []Schema{
	{Name: "claude", CLI: CLI{
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, NoTools: Native, NoPersist: Native, SafeMode: Native, DebugFile: Native,
			SystemPrompt: Native, AppendSystemPrompt: Native, Env: Native, OutputSchema: Native, Persona: Native},
		Events: []string{EventStart, EventText, EventToolUse, EventRetry, EventResult, EventError},
		Cost:   true, ConfigureHint: "run `claude-code configure agents`", LoginArgv: []string{"claude", "auth", "login"},
	}},
	{Name: "codex", CLI: CLI{
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, Autonomous: Native, NoPersist: Native, SafeMode: Native, Direct: Native, Env: Native,
			SystemPrompt: Native, AppendSystemPrompt: Native, OutputSchema: Native, Persona: Emulated},
		Events:       []string{EventStart, EventText, EventToolUse, EventToolResult, EventResult, EventError},
		ResumeFormat: "codex resume %s", ModelsProvider: "openai", LoginArgv: []string{"codex", "login"},
		ConfigureHint: "run `codex login` (ChatGPT) or `claude-code configure` → codex → API key",
	}},
	{Name: "gemini", CLI: CLI{
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, Autonomous: Native, DebugFile: Native, Direct: Native, Env: Native,
			SystemPrompt: Emulated, AppendSystemPrompt: Emulated, OutputSchema: Native, Persona: Emulated},
		Events: []string{EventStart, EventText, EventResult, EventError}, Cost: true,
		ResumeFormat: "agy --conversation %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
		ConfigureHint: "sign in with `agy` (Google login)", DisplayName: "antigravity", LoginArgv: []string{"agy"},
		ExtraUsage: [][2]string{{"bridge", "ingest files for a large-context task"}},
	}},
	{Name: "opencode", CLI: CLI{
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, Autonomous: Native, SafeMode: Native, Direct: Native, Env: Native,
			SystemPrompt: Native, AppendSystemPrompt: Emulated, OutputSchema: Emulated, Persona: Emulated},
		Events:       []string{EventStart, EventText, EventToolUse, EventToolResult, EventRetry, EventResult, EventError},
		ResumeFormat: "opencode run --session %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
		PromptInArgv: true, DirFlag: true, LoginArgv: []string{"opencode", "auth", "login"},
		ConfigureHint: "run `opencode auth login` (opencode Go) or `claude-code configure github` (Copilot)",
		CatalogUsage:  "model menu: list|add|remove|import|reset|check|sync",
		CatalogHint:   "Serving-only discovery: claude-code opencode catalog sync --profile P",
	}},
	{Name: "copilot", CLI: CLI{
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, Autonomous: Native, Memory: Native, Direct: Native, Env: Native,
			SystemPrompt: Emulated, AppendSystemPrompt: Emulated, OutputSchema: Emulated, Persona: Emulated},
		Events:       []string{EventStart, EventText, EventResult, EventError},
		ResumeFormat: "copilot --resume %s", ModelsProvider: "github", ReadNeedsTools: true,
		LoginArgv:     []string{"copilot", "login"},
		ConfigureHint: "run `claude-code configure github` (Copilot) or set an Anthropic/Gemini key",
	}},
}

// Roster returns independent copies of the roster rows, claude first, with external wire facts filled in.
func Roster() []Schema {
	rows := slices.Clone(rosterRows)
	for i := range rows {
		s := &rows[i]
		if spec, ok := providers.LookupAgentSpec(s.Name); ok && spec.Binary != "" {
			s.CLI.AgentSpec = spec
		}
		s.Tools = slices.Clone(s.Tools)
		s.CLI.Dialects = slices.Clone(s.CLI.Dialects)
		s.CLI.RouterSuffix = maps.Clone(s.CLI.RouterSuffix)
		s.CLI.Events = slices.Clone(s.CLI.Events)
		s.CLI.ModelsArgv = slices.Clone(s.CLI.ModelsArgv)
		s.CLI.LoginArgv = slices.Clone(s.CLI.LoginArgv)
		s.CLI.ExtraUsage = slices.Clone(s.CLI.ExtraUsage)
	}
	return rows
}

// LookupSchema returns a roster row by name, claude included.
func LookupSchema(name string) (Schema, bool) {
	for _, s := range Roster() {
		if s.Name == name {
			return s, true
		}
	}
	return Schema{}, false
}

// External reports whether the row drives an external CLI rather than the harness's Claude launcher.
func (s Schema) External() bool { return s.CLI.Binary != "" }

// ResumeHint renders the CLI's resume command, empty when the format or session id is empty.
func (s Schema) ResumeHint(id string) string {
	if s.CLI.ResumeFormat == "" || id == "" {
		return ""
	}
	return fmt.Sprintf(s.CLI.ResumeFormat, id)
}

func errUnknownAgent(name string) error { return fmt.Errorf("unknown agent %q", name) }

// ModelsProfile is the vendor's profile on its ModelsProvider that carries the vendor's own login.
func ModelsProfile(reg *providers.Registry, vendor string) (string, error) {
	s, _ := LookupSchema(vendor)
	for _, n := range reg.ProfilesOf(s.CLI.ModelsProvider) {
		if reg.Providers[n].Auth.Login == vendor {
			return n, nil
		}
	}
	return "", fmt.Errorf("%s: no %s profile with a %s login", vendor, s.CLI.ModelsProvider, vendor)
}
