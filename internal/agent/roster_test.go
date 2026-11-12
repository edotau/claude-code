package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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

// Register refuses a name outside the roster, before it touches the roster.
func TestRegisterPanicsOnUnknownAgent(t *testing.T) {
	defer func() {
		if r := recover(); r != "agent: Register bogus, not a roster agent" {
			t.Errorf("recovered %v, want the not-a-roster-agent panic", r)
		}
	}()
	Register(Agent{Row: Schema{Name: "bogus"}, Driver: fixtureDriver{NewBaseEngine("bogus")}})
}

// A shell model env beats the vault model pin for a CLI agent.
func TestAgentModelPinShellEnvBeatsVault(t *testing.T) {
	hermetic(t, `{"fallback": [], "providers": {}}`)
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Model: "vault-model"}); err != nil {
		t.Fatal(err)
	}
	if got := AgentModelPin("codex"); got != "vault-model" {
		t.Fatalf("vault pin = %q", got)
	}
	t.Setenv("CODEX_MODEL", "shell-model")
	if got := AgentModelPin("codex"); got != "shell-model" {
		t.Errorf("AgentModelPin = %q, want shell-model", got)
	}
}

func putRawPins(t *testing.T, pins map[string]string) {
	t.Helper()
	if err := vault.Update(func(tx *vault.Tx) error {
		for k, v := range pins {
			if err := tx.Put(vault.Pins, k, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Given unmigrated legacy pins in every shape, the records answer AgentActive and carry the same state.
func TestLegacyPinsDriveAgentActiveAndRecords(t *testing.T) {
	vaulttest.Use(t)
	putRawPins(t, map[string]string{
		"CODEX_PROFILE": "chatgpt", "COPILOT_PROFILE": "personal", "COPILOT_ACTIVE": "off",
		"COPILOT_GITHUB_USER": "alice", "GEMINI_PROFILE": "antigravity", "OPENCODE_MODEL": "m", "OPENCODE_ACTIVE": "off",
	})
	for agent, want := range map[string]bool{"claude": true, "codex": true, "gemini": true, "copilot": false, "opencode": false} {
		if got := AgentActive(agent); got != want {
			t.Errorf("AgentActive(%s) = %v, want %v", agent, got, want)
		}
	}
	if r := vault.GetAgentRecord("copilot"); r.Account != "alice" || r.Profile != "" {
		t.Errorf("copilot record = %+v, want account alice and no profile", r)
	}
	if r := vault.GetAgentRecord("opencode"); r.Model != "m" {
		t.Errorf("opencode record = %+v, want the model kept while off", r)
	}
}

// An unmigrated vault's raw pin makes the agent on and never reads as every agent off.
func TestUnmigratedRawPinIsNotReadAsOff(t *testing.T) {
	vaulttest.Use(t)
	putRawPins(t, map[string]string{"COPILOT_PROFILE": "dev"})
	if got := vault.GetAgentRecord("copilot").Profile; got != "dev" {
		t.Fatalf("copilot profile = %q, want dev", got)
	}
}

// SetAgentRecord round-trips, and an empty profile turns an external agent off.
func TestSetAgentRecordRoundTripAndOff(t *testing.T) {
	vaulttest.Use(t)
	if err := vault.SetAgentRecord("gemini", vault.AgentRecord{Profile: "antigravity", Model: "g"}); err != nil {
		t.Fatal(err)
	}
	if r := vault.GetAgentRecord("gemini"); r.Profile != "antigravity" || r.Model != "g" || !AgentActive("gemini") {
		t.Fatalf("gemini = %+v active=%v", r, AgentActive("gemini"))
	}
	if err := vault.SetAgentRecord("gemini", vault.AgentRecord{Model: "g"}); err != nil {
		t.Fatal(err)
	}
	if AgentActive("gemini") || vault.GetAgentRecord("gemini").Model != "g" {
		t.Fatalf("gemini with no profile must be off keeping its model: %+v", vault.GetAgentRecord("gemini"))
	}
}

// vault.OwnLoginProfiles must name the built-in rows with auth.login == agent, and opencode's Zen id.
func TestOwnLoginProfilesMatchBuiltInDefaults(t *testing.T) {
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range agentid.External() {
		want := vault.OwnLoginProfiles[a]
		if a == "opencode" {
			if want != "zen-free" {
				t.Errorf("opencode own login = %q, want %q", want, "zen-free")
			}
			continue
		}
		if !ownLogin(func() *providers.Registry { return reg }, a, want) {
			t.Errorf("%s: %q is not a built-in row with auth.login == %s", a, want, a)
		}
	}
}

func TestAgentPinsRejectUnreadableRecords(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{")
	pins, err := AgentPins()
	if err == nil || !strings.Contains(err.Error(), "agent pins: vault: agent-records is corrupt") || pins != nil {
		t.Fatalf("AgentPins = %v, %v; want no pins and the vault read error", pins, err)
	}
	if _, err := rosterPick(router.PickOpts{}); err == nil || !strings.Contains(err.Error(), "agent-records is corrupt") {
		t.Fatalf("RosterPick error = %v", err)
	}
}

func TestAgentPinsRejectVaultIOError(t *testing.T) {
	dir := vaulttest.Use(t)
	if err := os.WriteFile(filepath.Join(dir, "state"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	pins, err := AgentPins()
	var pathErr *os.PathError
	if pins != nil || !errors.As(err, &pathErr) {
		t.Fatalf("AgentPins = %v, %v; want no pins and the wrapped filesystem error", pins, err)
	}
}

// Ties agentModelEnv to the four external agents' model knobs; claude has none.
func TestAgentModelEnvPinsTheExternalAgents(t *testing.T) {
	want := map[string]string{"codex": "CODEX_MODEL", "copilot": "COPILOT_MODEL", "gemini": "GEMINI_MODEL", "opencode": "OPENCODE_MODEL"}
	if !reflect.DeepEqual(agentModelEnv, want) {
		t.Errorf("agentModelEnv = %v, want %v", agentModelEnv, want)
	}
}

const (
	sessProfile       = "profile-r7"
	sessGithubProfile = "profile-k3"
	sessGPT           = "gpt-6-1-sol"
	sessClaude        = "claude-sonnet-5-5"
)

// sessFix is a Databricks session profile plus a github copilot profile, with github already exhausted.
type sessFix struct {
	p           *providers.Provider
	tb          *router.RouteTable
	dbx, github string
	st          router.State
	opts        router.PickOpts
}

func newSessFix(t *testing.T) *sessFix {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", sessProfile)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{`+
		`"databricks":{"`+sessProfile+`":{"preset":"databricks","host":"https://profile.example.com",`+
		`"auth":{"type":"pat"},"models":{"sonnet":"`+sessClaude+`"}}},`+
		`"github":{"`+sessGithubProfile+`":{"protocol":"openai","base_url":"https://api.githubcopilot.com",`+
		`"auth":{"type":"cli","login":"copilot"},"models":{"sonnet":"auto"}}}}}`)
	if err := providers.SetOpencodeMenu([]providers.OpencodeModel{{ID: sessGPT, Label: "GPT", Responses: true}}); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers[sessProfile]
	if p == nil {
		t.Fatal("profile absent")
	}
	vaulttest.Secret(t, providers.AuthSecretName(p), "profile-auth")
	vaulttest.SetRecord(t, "copilot", "profile", sessGithubProfile)
	activateAgents(t, "codex", "opencode")
	tb, err := router.LoadFrom(reg, sessGithubProfile)
	if err != nil {
		t.Fatal(err)
	}
	dbx, github := router.ProviderAccount(sessProfile), router.ProviderAccount(sessGithubProfile)
	return &sessFix{p: p, tb: tb, dbx: dbx, github: github,
		st:   router.State{github: {UsedPct: 100, Seen: now0, ExhaustedUntil: now0.Add(time.Hour)}},
		opts: router.PickOpts{Configured: allConfigured, Now: func() time.Time { return now0 }, WorkRepo: func() bool { return true }}}
}

func (s *sessFix) pickAgent(t *testing.T, agent string, state router.State, opts router.PickOpts) router.PickResult {
	t.Helper()
	opts, err := rosterPick(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s.tb.Pick(context.Background(), "sonnet", agent, state, opts)
}

func (s *sessFix) models(r router.PickResult, agent, account string) []router.Candidate {
	var out []router.Candidate
	for _, c := range r.Candidates {
		if c.Agent == agent && c.Account == account {
			out = append(out, c)
		}
	}
	return out
}

func (s *sessFix) assertProfile(t *testing.T, c router.Candidate, agent, model string) {
	t.Helper()
	if c.Agent != agent || c.Account != s.dbx || c.Profile != sessProfile || c.Model != model {
		t.Errorf("agent = %+v, want %s %s profile %s model %s", c, agent, s.dbx, sessProfile, model)
	}
}

func (s *sessFix) skipped(r router.PickResult, agent, gate string) bool {
	return slices.ContainsFunc(r.Skipped, func(k router.Skip) bool {
		return strings.Split(k.Agent, "·")[0] == agent && k.Gate == gate
	})
}

func TestSessionProfileAgentsCopilot(t *testing.T) {
	s := newSessFix(t)
	r := s.pickAgent(t, "copilot", s.st, s.opts)
	if len(r.Candidates) == 0 {
		t.Fatalf("copilot absent: %+v", r)
	}
	s.assertProfile(t, r.Candidates[0], "copilot", router.DatabricksModel("copilot", "sonnet", s.p))
	if len(s.models(r, "copilot", s.github)) != 0 || !s.skipped(r, "copilot", router.GateExhausted) {
		t.Errorf("copilot github profile = %+v", r)
	}
}

func TestSessionProfileAgentsCodex(t *testing.T) {
	s := newSessFix(t)
	r := s.pickAgent(t, "codex", s.st, s.opts)
	if len(r.Candidates) < 3 {
		t.Fatalf("codex/copilot absent: %+v", r)
	}
	own, _ := s.tb.Cell("codex", "sonnet")
	if c := r.Candidates[0]; c.Agent != "codex" || c.Account != own.Account || c.Model != own.Model || c.Profile != "" {
		t.Errorf("codex = %+v, want %+v", c, own)
	}
	s.assertProfile(t, r.Candidates[1], "codex", providers.FirstResponsesModel(s.p))
	s.assertProfile(t, r.Candidates[2], "copilot", r.Candidates[1].Model)
	if n := len(s.models(r, "copilot", s.dbx)); n != 1 {
		t.Errorf("copilot rows on the session account = %d, want 1: %+v", n, r.Candidates)
	}
	seen := map[string]bool{}
	for _, c := range s.models(r, "copilot", s.dbx) {
		if seen[c.Model] {
			t.Errorf("copilot profile model repeated: %+v", c)
		}
		seen[c.Model] = true
	}
}

func TestSessionProfileAgentsCopilotAuth(t *testing.T) {
	s := newSessFix(t)
	blocked := s.opts
	blocked.Configured = func(agent string, c router.Cell) error {
		if agent == "copilot" && c.Account == s.dbx {
			return errors.New("profile auth")
		}
		return nil
	}
	for _, agent := range []string{"copilot", "codex"} {
		r := s.pickAgent(t, agent, s.st, blocked)
		if len(s.models(r, "copilot", s.dbx)) != 0 || !s.skipped(r, "copilot", router.GateAvailable) {
			t.Errorf("%s copilot profile auth = %+v", agent, r)
		}
	}
}

func TestSessionProfileAgentsCopilotOff(t *testing.T) {
	s := newSessFix(t)
	vaulttest.SetRecord(t, "copilot", "profile", "")
	if r := s.pickAgent(t, "codex", s.st, s.opts); slices.ContainsFunc(r.Candidates, func(c router.Candidate) bool { return c.Agent == "copilot" }) {
		t.Errorf("copilot off = %+v", r)
	}
}

func TestSessionProfileAgentsProfileAuth(t *testing.T) {
	s := newSessFix(t)
	for _, state := range []struct {
		gate  string
		entry router.Entry
	}{
		{router.GateAuth, router.Entry{AuthAt: now0, AuthReason: "profile auth"}},
		{router.GateExhausted, router.Entry{Seen: now0, ExhaustedUntil: now0.Add(time.Hour)}},
	} {
		blocked := router.State{s.github: s.st[s.github], s.dbx: state.entry}
		for _, agent := range []string{"copilot", "codex"} {
			r := s.pickAgent(t, agent, blocked, s.opts)
			if len(s.models(r, "copilot", s.dbx)) != 0 || !s.skipped(r, "copilot", state.gate) {
				t.Errorf("%s copilot profile %s = %+v", agent, state.gate, r)
			}
		}
	}
}

func TestSessionProfileAgentsCopilotProfile(t *testing.T) {
	s := newSessFix(t)
	vaulttest.SetRecord(t, "copilot", "profile", sessProfile)
	only := s.opts
	only.Fallback = router.FallbackNone
	r := s.pickAgent(t, "copilot", s.st, only)
	if len(r.Candidates) != 1 {
		t.Fatalf("copilot profile = %+v", r)
	}
	s.assertProfile(t, r.Candidates[0], "copilot", router.DatabricksModel("copilot", "sonnet", s.p))
}

func TestSessionProfileAgentsCopilotModel(t *testing.T) {
	s := newSessFix(t)
	cell := s.tb.Rows["copilot"]["sonnet"]
	cell.Account, cell.Model = s.dbx, s.p.WireModel(s.p.Models["sonnet"])
	s.tb.Rows["copilot"]["sonnet"] = cell
	only := s.opts
	only.Fallback = router.FallbackNone
	if r := s.pickAgent(t, "copilot", s.st, only); len(r.Candidates) != 1 || r.Candidates[0].Cell != cell {
		t.Errorf("copilot profile model = %+v, want %+v", r, cell)
	}
}

func TestSessionProfileAgentsCodexHopModel(t *testing.T) {
	s := newSessFix(t)
	own, _ := s.tb.Cell("codex", "sonnet")
	own.Effort = "high"
	s.tb.Rows["codex"]["sonnet"] = own
	menu := []providers.OpencodeModel{{ID: sessGPT, Label: "GPT", Responses: true}, {ID: own.Model, Label: "Own", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	r := s.pickAgent(t, "codex", s.st, s.opts)
	hops := 0
	for _, c := range r.Candidates {
		if c.Account != s.dbx || c.Agent == "claude" {
			continue
		}
		hops++
		if c.Model != s.p.WireModel(own.Model) || c.Effort != "high" {
			t.Errorf("hop = %+v, want model %s effort high", c, s.p.WireModel(own.Model))
		}
	}
	if hops != 2 {
		t.Errorf("hops = %d, want 2: %+v", hops, r.Candidates)
	}
}

// A candidate dropped after the pick must not lend its slot to the next survivor of the same agent and account.
func TestPreviewOutcomeMatchesByRungNotPosition(t *testing.T) {
	cell := func(m string) router.Cell { return router.Cell{Model: m} }
	pick := router.PickResult{Candidates: []router.Candidate{
		{Agent: "codex", Cell: cell("m1"), Profile: "A"},
		{Agent: "codex", Cell: cell("m2"), Profile: "B"},
	}}
	chain := Chain{Dispatches: []Dispatch{{Agent: &pinFallbackRunner{name: "codex"}, Model: "m2", rung: 2}}}
	got := previewOutcome(chain, pick).Candidates
	if len(got) != 1 || got[0].Profile != "B" || got[0].Model != "m2" {
		t.Fatalf("candidates = %+v, want only rung 2 (profile B, model m2)", got)
	}
}

// The roster survives a providers.Load() failure and no longer lists the folded-in "api" agent.
func TestListAgentsRegistryLoadErrorNoPanic(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(providers.EnvSessionProfile, "")
	vaulttest.Put(t, vault.Overlay, "providers", "{not valid json")
	if _, err := providers.Load(); err == nil {
		t.Fatal("providers.Load() must fail on malformed overlay JSON for this test to prove anything")
	}

	var lines []string
	for _, r := range Summaries() { // must not panic despite a nil registry from the failed Load()
		lines = append(lines, r.Line())
	}
	if out := strings.Join(lines, "\n"); !strings.Contains(out, " claude ") {
		t.Errorf("no claude row in output:\n%s", out)
	}
	for _, line := range lines {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == "api" {
			t.Errorf("api agent still listed: %q", line)
		}
	}
}
