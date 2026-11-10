package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// fixtureDriver stands in for a vendor driver, which this package cannot import: base defaults, no CLI to exec.
type fixtureDriver struct{ BaseEngine }

func (fixtureDriver) Exec(context.Context, CLIPrep, RunOptions, wire.Sink) (wire.Result, error) {
	return wire.Result{}, errors.New("fixture driver: no CLI")
}

// fixtureCopilot refuses a missing route like copilotEngine.NoRoute (internal/copilot/copilot.go:279).
type fixtureCopilot struct{ fixtureDriver }

// DescribeRoute is copilot's no-route terminal (internal/copilot/route.go:83); the fixture has no route to find.
func (fixtureCopilot) DescribeRoute(*providers.Registry) RouteInfo {
	return RouteInfo{Agent: "copilot", Route: RouteNone, Reason: "no route for copilot: " + SchemaFor("copilot").CLI.ConfigureHint}
}

func (e fixtureCopilot) NoRoute(reg *providers.Registry, _ providers.Target) error {
	return RouteErr(e.DescribeRoute(reg))
}

// fixtureGemini keeps agy on its Google login like geminiEngine.FollowsPin (internal/gemini/gemini.go:110).
type fixtureGemini struct{ fixtureDriver }

func (fixtureGemini) FollowsPin(p *providers.Provider) bool {
	recs, err := vault.ReadAgentRecords([]string{"gemini"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gemini: vault unreadable, pins ignored: %v\n", err)
	}
	return !GeminiGatewayRow(p) || recs["gemini"].Profile == p.Name
}

// NoRoute refuses an unpinned Databricks gateway row like geminiEngine.NoRoute (internal/gemini/gemini.go:115).
func (fixtureGemini) NoRoute(_ *providers.Registry, t providers.Target) error {
	if GeminiGatewayRow(t.Provider) {
		return errors.New("gemini: agy needs a Google login (agy auth login) or a GEMINI_PROFILE pin")
	}
	return nil
}

// fixtureHub mirrors opencode's Hub (internal/opencode/register.go:11-12, hub.go) minus the opencode login probe.
var fixtureHub = &Hub{
	Start: func(reg *providers.Registry, agentName, explicitProvider string) (*providers.Provider, error) {
		if explicitProvider != "" {
			p, err := reg.Get(explicitProvider)
			if err != nil {
				return nil, err
			}
			if p.OpencodeID == "" {
				return nil, fmt.Errorf("%s: provider %s has no opencodeID, so the hub cannot route to it", agentName, p.Name)
			}
			return p, nil
		}
		for _, id := range reg.Routing.Opencode.LadderOrder() {
			for _, name := range reg.Names() {
				if p := reg.Providers[name]; p.OpencodeID == id {
					return p, nil
				}
			}
			if fixtureOwnPlan(id) {
				return &providers.Provider{Name: id, OpencodeID: id}, nil
			}
		}
		return nil, fmt.Errorf("%s: no opencode ladder configured (set opencodeLadder with `claude-code vault edit`)", agentName)
	},
	Dispatch: func(agentName string, p *providers.Provider, _ *providers.Registry) HubEngine {
		return fixtureHubEngine{agent: agentName, provider: p}
	},
	OwnPlan: fixtureOwnPlan,
}

// fixtureOwnPlan is opencode's OpencodeCredID(id) == "opencode" (internal/opencode/opencode.go:621).
func fixtureOwnPlan(id string) bool { return id == "opencode-go" || id == "opencode" }

// fixtureHubEngine is a hub step with no opencode CLI behind it.
type fixtureHubEngine struct {
	agent    string
	provider *providers.Provider
}

func (h fixtureHubEngine) Name() string                         { return h.agent }
func (h fixtureHubEngine) HubProvider() *providers.Provider     { return h.provider }
func (h fixtureHubEngine) WithFirstOutput(time.Duration) Engine { return h }
func (fixtureHubEngine) Caps() Caps                             { return SchemaFor("opencode").CLI.Caps }
func (fixtureHubEngine) Available() error                       { return errors.New("fixture hub: no CLI") }
func (fixtureHubEngine) Run(context.Context, RunOptions, wire.Sink) (wire.Result, error) {
	return wire.Result{}, errors.New("fixture hub: no CLI")
}

// fixtureRows copy each vendor's register row verbatim; vendor constants are inlined as literals.
var fixtureRows = []Schema{
	// internal/claude/register.go
	{Name: "claude", CLI: CLI{
		PromptVia: PromptStdin,
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, NoTools: Native, Autonomous: Native, NoPersist: Native, SafeMode: Native, DebugFile: Native,
			SystemPrompt: Native, AppendSystemPrompt: Native, Env: Native, OutputSchema: Native, Persona: Native,
			Permissions: Native},
		Events: []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventRetry, wire.EventResult, wire.EventError},
		Cost:   true, ConfigureHint: "run `claude-code configure agents`", LoginArgv: []string{"claude", "auth", "login"},
	}},
	// internal/codex/register.go
	{Name: "codex", CLI: CLI{
		PromptVia: PromptStdin,
		Binary:    "codex", Dialects: []string{providers.ProtocolResponses},
		RouterSuffix: map[string]string{providers.ProtocolResponses: "/" + providers.ProtocolResponses},
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, Autonomous: Native, NoPersist: Native, SafeMode: Native, Direct: Native, Env: Native,
			SystemPrompt: Native, AppendSystemPrompt: Native, OutputSchema: Native, Persona: Emulated,
			Permissions: Native},
		Events:       []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventToolResult, wire.EventResult, wire.EventError},
		ResumeFormat: "codex resume %s", ModelsProvider: "openai", LoginArgv: []string{"codex", "login"},
		ConfigureHint: "run `codex login` (ChatGPT) or `claude-code configure` → codex → API key",
	}},
	// internal/copilot/copilot.go; MinVersion is copilot.MinMultiProviderCLI
	{Name: "copilot", CLI: CLI{
		PromptVia: PromptMessage,
		Binary:    "copilot", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolOpenAI},
		RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI,
			providers.ProtocolResponses: "/" + providers.ProtocolResponses},
		MinVersion: "1.0.104", MinReason: "named-provider model registry needs it",
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, ReadOnly: Native, Autonomous: Native, Fleet: Native, Memory: Native, Direct: Native, Env: Native, Permissions: Native,
			SystemPrompt: Native, AppendSystemPrompt: Native, OutputSchema: Emulated, Persona: Emulated},
		Events:       []string{wire.EventStart, wire.EventText, wire.EventProgress, wire.EventResult, wire.EventError},
		ResumeFormat: "copilot --resume %s", ModelsProvider: "github", ReadNeedsTools: true,
		LoginArgv:     []string{"copilot", "login"},
		ConfigureHint: "run `claude-code configure github` (Copilot) or set an Anthropic/Gemini key",
	}},
	// internal/gemini/register.go
	{Name: "gemini", CLI: CLI{
		PromptVia:      PromptStdin,
		PromptMaxBytes: 100 * 1024,
		PackVia:        PackFile,
		Binary:         "agy",
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, ReadOnly: Emulated, Autonomous: Native, DebugFile: Native, Direct: Native, Env: Native,
			SystemPrompt: Emulated, AppendSystemPrompt: Emulated, OutputSchema: Native, Persona: Emulated,
			Permissions: Native},
		Events: []string{wire.EventStart, wire.EventText, wire.EventProgress, wire.EventResult, wire.EventError}, Cost: true,
		ResumeFormat: "agy --conversation %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
		ConfigureHint: "sign in with `agy` (Google login)", DisplayName: "antigravity", LoginArgv: []string{"agy"},
	}},
	// internal/opencode/register.go; OwnLoginProfile is opencode.OwnLogin
	{Name: "opencode", CLI: CLI{
		Binary: "opencode", Dialects: []string{providers.ProtocolAnthropic, providers.ProtocolGemini, providers.ProtocolOpenAI},
		RouterSuffix: map[string]string{providers.ProtocolAnthropic: "", providers.ProtocolGemini: "/" + providers.ProtocolGemini,
			providers.ProtocolOpenAI: "/" + providers.ProtocolOpenAI, providers.ProtocolResponses: "/" + providers.ProtocolResponses},
		MaxVersion: "1.x", MaxReason: "2.x broke the harness opencode route",
		Caps: Caps{Model: Native, Provider: Native, Effort: Native, WorkDir: Native, SessionID: Native,
			Tools: Native, Autonomous: Native, SafeMode: Native, Direct: Native, Env: Native,
			SystemPrompt: Native, Permissions: Native, AppendSystemPrompt: Emulated, OutputSchema: Emulated, Persona: Emulated},
		Events:       []string{wire.EventStart, wire.EventText, wire.EventToolUse, wire.EventToolResult, wire.EventRetry, wire.EventResult, wire.EventError},
		ResumeFormat: "opencode run --session %s", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
		PromptVia: PromptArgv, OwnLoginProfile: "zen-free", PinIDs: providers.RouteOpencodeSubIDs, DirFlag: true, Hub: true, LoginArgv: []string{"opencode", "auth", "login"},
		ConfigureHint: "run `opencode auth login` (opencode Go) or `claude-code configure github` (Copilot)",
		CatalogUsage:  "model menu: list|add|remove|import|reset|check|sync",
		CatalogHint:   "Serving-only discovery: claude-code opencode catalog sync --profile P",
	}},
}

// registerFixtureRoster registers fixtureRows and fails when the roster does not then list all five vendors.
func registerFixtureRoster() error {
	for _, r := range fixtureRows {
		a := Agent{Row: r, Driver: fixtureDriver{NewBaseEngine(r.Name)}}
		switch r.Name {
		case "copilot":
			a.Driver = fixtureCopilot{fixtureDriver{NewBaseEngine(r.Name)}}
		case "gemini":
			a.Driver = fixtureGemini{fixtureDriver{NewBaseEngine(r.Name)}}
		case "opencode":
			a.Hub = fixtureHub
		}
		Register(a)
	}
	if got := Names(); !slices.Equal(got, []string{"codex", "copilot", "gemini", "opencode"}) {
		return fmt.Errorf("fixture roster: Names() = %v, want [codex copilot gemini opencode]", got)
	}
	for _, name := range []string{"claude", "codex", "copilot", "gemini", "opencode"} {
		if _, ok := LookupSchema(name); !ok {
			return fmt.Errorf("fixture roster: LookupSchema(%q) not found", name)
		}
	}
	return nil
}

func TestSchemaResumeHintEmpty(t *testing.T) {
	for _, s := range Roster() {
		if got := s.ResumeHint(""); got != "" {
			t.Errorf("schema %q empty session resume = %q", s.Name, got)
		}
		if !s.External() && s.ResumeHint("session") != "" {
			t.Errorf("schema %q without a resume format rendered a command", s.Name)
		}
	}
	if got := (Schema{}).ResumeHint("session"); got != "" {
		t.Errorf("zero schema resume = %q", got)
	}
}

func TestRosterReturnsCopies(t *testing.T) {
	want := Roster()
	rows := Roster()
	for i := range rows {
		s := &rows[i]
		s.Name = "changed"
		s.CLI.Caps.Model = None
		if len(s.Tools) > 0 {
			s.Tools[0] = "changed"
		}
		for _, values := range [][]string{s.CLI.Dialects, s.CLI.Events, s.CLI.ModelsArgv, s.CLI.LoginArgv} {
			if len(values) > 0 {
				values[0] = "changed"
			}
		}
		if len(s.CLI.ExtraUsage) > 0 {
			s.CLI.ExtraUsage[0][0] = "changed"
		}
		if s.CLI.RouterSuffix != nil {
			s.CLI.RouterSuffix["changed"] = "changed"
		}
	}
	if got := Roster(); !reflect.DeepEqual(got, want) {
		t.Error("mutating a roster changed later calls")
	}
	s, _ := LookupSchema("opencode")
	s.CLI.Events[0] = "changed"
	s.CLI.ModelsArgv[0] = "changed"
	s.CLI.LoginArgv[0] = "changed"
	s.CLI.Dialects[0] = "changed"
	s.CLI.RouterSuffix["changed"] = "changed"
	if got := Roster(); !reflect.DeepEqual(got, want) {
		t.Error("mutating a lookup changed later calls")
	}
}

func TestLookupSchemaPinIDsAreIndependent(t *testing.T) {
	s, ok := LookupSchema("opencode")
	if !ok || len(s.CLI.PinIDs) == 0 {
		t.Fatal("opencode must expose at least one extra pin ID")
	}
	original := s.CLI.PinIDs[0]
	t.Cleanup(func() { s.CLI.PinIDs[0] = original })
	s.CLI.PinIDs[0] = "bogus"
	if err := ValidAgentPin(nil, "opencode", "bogus"); err == nil {
		t.Fatal("mutating lookup PinIDs made bogus a valid opencode pin")
	}
	if err := ValidAgentPin(nil, "opencode", original); err != nil {
		t.Fatalf("mutating lookup PinIDs invalidated original pin %q: %v", original, err)
	}
}

// A pinned profile read through a registry that cannot load is an error, not a silent "not an own-login pin".
