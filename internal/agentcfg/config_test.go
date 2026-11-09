package agentcfg

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// setup has Databricks rows dev (signed in) and dev2, and claude pinned to max.
func setup(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}},"dev2":{"preset":"databricks",`+
		`"host":"https://dev2.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	vaulttest.SetRecord(t, "claude", "profile", "max")
	t.Setenv(providers.EnvSessionProfile, "")
}

func pin(t *testing.T, agent, profile, fallback string) {
	t.Helper()
	err := vault.UpdateAgentRecords(func(m map[string]vault.AgentRecord) {
		r := m[agent]
		r.Profile, r.Fallback, r.Model = profile, fallback, "m1"
		m[agent] = r
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFor(t *testing.T) {
	cases := []struct {
		name, agent, session string
		l                    Launch
		want, fallback, err  string
	}{
		{"codex pin", "codex", "", Launch{}, "dev2", "", ""},
		{"copilot pin", "copilot", "", Launch{}, "dev2", "", ""},
		{"gemini pin", "gemini", "", Launch{}, "dev2", "", ""},
		{"profile beats pin", "codex", "", Launch{Profile: "dev"}, "dev", "", ""},
		{"profile session", "codex", "dev", Launch{Profile: "session"}, "dev", "", ""},
		{"provider databricks", "gemini", "", Launch{Provider: "databricks"}, "dev", "", ""},
		{"provider uses signed-in databricks session", "codex", "dev", Launch{Provider: "databricks"}, "dev", "", ""},
		{"provider skips unsigned databricks session", "codex", "dev2", Launch{Provider: "databricks"}, "dev", "", ""},
		{"fallback carried", "codex", "", Launch{}, "dev2", "dev", ""},
		{"opencode own login", "opencode", "", Launch{Profile: "zen-free"}, "zen-free", "", ""},
		{"opencode sub id", "opencode", "", Launch{Profile: "opencode-go"}, "opencode-go", "", ""},
		{"unsupported provider", "codex", "", Launch{Provider: "anthropic"}, "", "", "--provider anthropic"},
		{"unknown profile", "codex", "", Launch{Profile: "nosuch"}, "", "", "nosuch"},
		{"stale fallback ignored", "codex", "", Launch{}, "dev2", "gone", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setup(t)
			pin(t, c.agent, "dev2", c.fallback)
			t.Setenv(providers.EnvSessionProfile, c.session)
			got, err := For(c.agent, c.l)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil || got.Profile != c.want || got.Model != "m1" || got.Fallback != c.fallback {
				t.Fatalf("For(%s) = %+v, %v; want profile %s", c.agent, got, err, c.want)
			}
		})
	}
}

func TestForGeminiUnpinnedStaysOnLogin(t *testing.T) {
	setup(t)
	got, err := For("gemini", Launch{})
	if err != nil || got.Profile != "antigravity" {
		t.Fatalf("For(gemini) = %+v, %v", got, err)
	}
}

func TestForOpencodePinWithoutRegistryRow(t *testing.T) {
	setup(t)
	pin(t, "opencode", "zen-free", "")
	got, err := For("opencode", Launch{})
	if err != nil || got.Profile != "zen-free" {
		t.Fatalf("For(opencode) = %+v, %v", got, err)
	}
}

// --profile session with no session profile and no claude record keeps the pin instead of blanking it.
func TestForSessionWithoutLeadKeepsPin(t *testing.T) {
	setup(t)
	vaulttest.SetRecord(t, "claude", "profile", "")
	pin(t, "codex", "dev2", "")
	got, err := For("codex", Launch{Profile: "session"})
	if err != nil || got.Profile != "dev2" {
		t.Fatalf("For(codex, session) = %+v, %v; want the dev2 pin", got, err)
	}
}

// No stored arm: a leftover HARNESS_DATABRICKS_ARM in the env never moves an agent off its pin.
func TestForIgnoresArmEnv(t *testing.T) {
	setup(t)
	pin(t, "gemini", "", "")
	t.Setenv("HARNESS_DATABRICKS_ARM", "dev")
	got, err := For("gemini", Launch{})
	if err != nil || got.Profile != "antigravity" {
		t.Fatalf("For(gemini) with arm env = %+v, %v; want the antigravity pin", got, err)
	}
}

// An unsupported --provider is rejected even when --profile is also given.
func TestForRejectsBadProviderWithProfile(t *testing.T) {
	setup(t)
	for _, profile := range []string{"dev", "session"} {
		_, err := For("codex", Launch{Profile: profile, Provider: "openai"})
		if err == nil || !strings.Contains(err.Error(), "--provider openai") {
			t.Fatalf("For(profile %s, provider openai) err = %v, want the provider rejection", profile, err)
		}
	}
}
