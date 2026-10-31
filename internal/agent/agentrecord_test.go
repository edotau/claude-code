package agent

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
	if r := GetAgentRecord("copilot"); r.Account != "alice" || r.Profile != "" {
		t.Errorf("copilot record = %+v, want account alice and no profile", r)
	}
	if r := GetAgentRecord("opencode"); r.Model != "m" {
		t.Errorf("opencode record = %+v, want the model kept while off", r)
	}
}

// An unmigrated vault's raw pin makes the agent on and never reads as every agent off.
func TestUnmigratedRawPinIsNotReadAsOff(t *testing.T) {
	vaulttest.Use(t)
	putRawPins(t, map[string]string{"COPILOT_PROFILE": "dev"})
	if got := GetAgentRecord("copilot").Profile; got != "dev" {
		t.Fatalf("copilot profile = %q, want dev", got)
	}
}

// SetAgentRecord round-trips, and an empty profile turns an external agent off.
func TestSetAgentRecordRoundTripAndOff(t *testing.T) {
	vaulttest.Use(t)
	if err := SetAgentRecord("gemini", AgentRecord{Profile: "antigravity", Model: "g"}); err != nil {
		t.Fatal(err)
	}
	if r := GetAgentRecord("gemini"); r.Profile != "antigravity" || r.Model != "g" || !AgentActive("gemini") {
		t.Fatalf("gemini = %+v active=%v", r, AgentActive("gemini"))
	}
	if err := SetAgentRecord("gemini", AgentRecord{Model: "g"}); err != nil {
		t.Fatal(err)
	}
	if AgentActive("gemini") || GetAgentRecord("gemini").Model != "g" {
		t.Fatalf("gemini with no profile must be off keeping its model: %+v", GetAgentRecord("gemini"))
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
			if want != OpencodeOwnLogin {
				t.Errorf("opencode own login = %q, want %q", want, OpencodeOwnLogin)
			}
			continue
		}
		if !ownLogin(reg, a, want) {
			t.Errorf("%s: %q is not a built-in row with auth.login == %s", a, want, a)
		}
	}
}
