package agent

import (
	"reflect"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRosterMatchesSpecs(t *testing.T) {
	var names, runners []string
	for _, s := range Roster() {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"claude", "codex", "copilot", "gemini", "opencode"}) {
		t.Fatalf("roster order = %v", names)
	}
	for _, r := range Engines() {
		runners = append(runners, r.Name())
	}
	for label, other := range map[string][]string{
		"Engines": runners, "RouteAgents": slices.Clone(providers.RouteAgents),
	} {
		slices.Sort(other)
		want := slices.Clone(names)
		slices.Sort(want)
		if !slices.Equal(other, want) {
			t.Errorf("%s names = %v, roster names = %v", label, other, want)
		}
	}
}

func TestRosterFacts(t *testing.T) {
	catalogs := map[string]CLI{
		"codex": {ResumeFormat: "codex resume session", ModelsProvider: "openai"},
		"gemini": {ResumeFormat: "agy --conversation session", ModelsArgv: []string{"models"}, ReadNeedsTools: true,
			ExtraUsage: [][2]string{{"bridge", "ingest files for a large-context task"}}},
		"opencode": {ResumeFormat: "opencode run --session session", ModelsArgv: []string{"models"}, ReadNeedsTools: true},
		"copilot":  {ResumeFormat: "copilot --resume session", ModelsProvider: "github", ReadNeedsTools: true},
	}
	for _, s := range Roster() {
		t.Run(s.Name, func(t *testing.T) {
			got, ok := LookupSchema(s.Name)
			if !ok || !reflect.DeepEqual(got, s) {
				t.Fatalf("LookupSchema(%q) = %+v, %v; want %+v", s.Name, got, ok, s)
			}
			if s.External() != (s.Name != "claude") {
				t.Errorf("External() = %v", s.External())
			}
			if s.Name == "claude" {
				if s.CLI.Binary != "" || s.CLI.Dialects != nil || s.CLI.RouterSuffix != nil {
					t.Errorf("claude wire facts = %+v", s.CLI)
				}
				return
			}
			want := catalogs[s.Name]
			if s.ResumeHint("session") != want.ResumeFormat {
				t.Error("resume command changed")
			}
			if !slices.Equal(s.CLI.ModelsArgv, want.ModelsArgv) || s.CLI.ModelsProvider != want.ModelsProvider ||
				s.CLI.ReadNeedsTools != want.ReadNeedsTools || !slices.Equal(s.CLI.ExtraUsage, want.ExtraUsage) {
				t.Error("catalog, tools or help facts changed")
			}
		})
	}
	if s, ok := LookupSchema("unknown"); ok || !reflect.DeepEqual(s, Schema{}) {
		t.Errorf("unknown schema = %+v, %v", s, ok)
	}
	if (Schema{}).External() {
		t.Error("zero schema is external")
	}
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

func TestLookupUnknownVendor(t *testing.T) {
	if s, ok := LookupSchema("nonexistent-vendor"); ok || s.Name != "" {
		t.Errorf("unknown schema = (%+v, %v), want (Schema{}, false)", s, ok)
	}
}

func TestKnownIncludesClaude(t *testing.T) {
	for _, s := range Roster() {
		if _, ok := LookupSchema(s.Name); !ok {
			t.Errorf("LookupSchema(%q) = false", s.Name)
		}
	}
	if _, ok := LookupSchema("nonexistent-vendor"); ok {
		t.Error("unknown agent is known")
	}
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
	for _, a := range vault.LegacyAgents[1:] {
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
