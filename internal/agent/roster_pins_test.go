package agent

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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

func TestAgentPinsRejectUnreadableRecords(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, vault.AgentRecordsKey, "{")
	pins, err := AgentPins()
	if err == nil || !strings.Contains(err.Error(), "agent pins: vault: agent-records is corrupt") || pins != nil {
		t.Fatalf("AgentPins = %v, %v; want no pins and the vault read error", pins, err)
	}
	if _, err := RosterPick(router.PickOpts{}); err == nil || !strings.Contains(err.Error(), "agent-records is corrupt") {
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
