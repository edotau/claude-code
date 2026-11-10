package agentconfig

import (
	"errors"
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
		{"unserved provider", "codex", "", Launch{Provider: "nosuchprov"}, "", "", "no signed-in nosuchprov profile serves codex"},
		{"unknown profile", "codex", "", Launch{Profile: "nosuch"}, "", "", "nosuch"},
		{"stale fallback ignored", "codex", "", Launch{}, "dev2", "gone", ""},
		{"last-used beats pin", "codex", "", Launch{LastProfile: "dev"}, "dev", "", ""},
		{"profile beats last-used", "codex", "", Launch{Profile: "dev", LastProfile: "openai"}, "dev", "", ""},
		{"invalid last-used keeps pin", "codex", "", Launch{LastProfile: "gone"}, "dev2", "", ""},
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

// A --provider other than the profile's is rejected even when --profile is also given.
func TestForRejectsBadProviderWithProfile(t *testing.T) {
	setup(t)
	t.Setenv(providers.EnvSessionProfile, "dev")
	for profile, want := range map[string]string{"dev": "--profile dev is not a openai profile", "session": "no signed-in openai profile serves codex"} {
		_, err := For("codex", Launch{Profile: profile, Provider: "openai", CanServe: databricksOnly})
		if err == nil || err.Error() != "agent codex: "+want {
			t.Fatalf("For(profile %s, provider openai) err = %v, want %q", profile, err, want)
		}
	}
}

// A non-routed unknown --profile with --provider wraps ErrUnknownProfile, like the same profile without --provider.
func TestForUnknownProfileWithProviderWrapsSentinel(t *testing.T) {
	setup(t)
	for _, provider := range []string{"", "databricks"} {
		_, err := For("codex", Launch{Profile: "nosuch", Provider: provider, CanServe: databricksOnly})
		if !errors.Is(err, ErrUnknownProfile) {
			t.Errorf("For(profile nosuch, provider %q) err = %v, want ErrUnknownProfile", provider, err)
		}
	}
}

func TestForRouted(t *testing.T) {
	setup(t)
	pin(t, "codex", "dev2", "")
	cases := []struct{ env, agent, profile, provider, want, err string }{
		{"", "codex", "", "", "", ""},
		{"dev", "codex", "session", "", "dev", ""},
		{"", "codex", "session", "", "max", ""},
		{"", "codex", "dev", "databricks", "dev", ""},
		{"", "codex", "max", "databricks", "", "not a databricks profile"},
		{"", "codex", "session", "databricks", "dev", ""},
		{"", "codex", "", "github", "", "no signed-in github profile serves codex"},
		{"", "codex", "nosuch", "", "", "not in the registry"},
		{"dev", "gemini", "session", "", "", ""},
		{"dev", "gemini", "dev", "", "dev", ""},
	}
	for _, c := range cases {
		t.Setenv(providers.EnvSessionProfile, c.env)
		cfg, err := For(c.agent, Launch{Profile: c.profile, Provider: c.provider, Routed: true, CanServe: databricksOnly})
		if cfg.Profile != c.want || (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%+v: got %q, %v", c, cfg.Profile, err)
		}
	}
	cfg, err := For("codex", Launch{})
	if err != nil || cfg.Profile != "dev2" {
		t.Errorf("non-routed pin = %q, %v, want dev2", cfg.Profile, err)
	}
}

// A --provider github with CanServe rejecting every row reaches the registry's github profiles and still fails.
func TestForRoutedProviderEvaluatesRegistryProfiles(t *testing.T) {
	setup(t)
	var seen []string
	deny := func(_ string, p *providers.Provider) bool { seen = append(seen, p.Provider); return false }
	_, err := For("codex", Launch{Provider: "github", Routed: true, CanServe: deny})
	if err == nil || !strings.Contains(err.Error(), "no signed-in github profile serves codex") || len(seen) == 0 {
		t.Fatalf("err = %v, evaluated %v; want a github row evaluated then rejected", err, seen)
	}
}

// databricksOnly stands in for agent.CanServe: only databricks rows serve the test agents.
func databricksOnly(_ string, p *providers.Provider) bool { return p.Provider == "databricks" }

// A non-routed --provider with no CanServe never picks a signed-in non-databricks profile (openai's chatgpt login is always present).
func TestForNonRoutedProviderWithoutCanServeRejectsNonDatabricks(t *testing.T) {
	setup(t)
	got, err := For("codex", Launch{Provider: "openai"})
	if err == nil || !strings.Contains(err.Error(), "no signed-in openai profile serves codex") {
		t.Fatalf("For(codex, provider openai) = %+v, %v; want the no-profile-serves error", got, err)
	}
}

// A --provider with no --profile picks the same profile routed and non-routed (one rule set), not the pin.
func TestForProviderPickMatchesRouted(t *testing.T) {
	setup(t)
	pin(t, "codex", "max", "")
	t.Setenv(providers.EnvSessionProfile, "dev")
	routed, err := For("codex", Launch{Provider: "databricks", Routed: true, CanServe: databricksOnly})
	if err != nil || routed.Profile == "" || routed.Profile == "max" {
		t.Fatalf("routed = %+v, %v; want a databricks pick", routed, err)
	}
	got, err := For("codex", Launch{Provider: "databricks"})
	if err != nil || got.Profile != routed.Profile {
		t.Fatalf("non-routed = %+v, %v; want %q like routed", got, err, routed.Profile)
	}
}
