package ask

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// A vault-pinned HARNESS_COWORKER arms its workspace when neither --overlay nor the process env names one.
func TestApplyCoworkerVaultEnvSource(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"qa":{"preset":"databricks",`+
		`"host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	vaulttest.Put(t, vault.Env, router.EnvCoworker, "databricks:qa")
	t.Setenv(router.EnvCoworker, "") // ApplyCoworker exports into this process: restore after the test
	t.Setenv(providers.EnvSessionProfile, "")
	if err := ApplyCoworker("", "", "codex", "", router.FallbackAll); err != nil {
		t.Fatalf("ApplyCoworker = %v", err)
	}
	if got, prof := os.Getenv(router.EnvCoworker), os.Getenv(providers.EnvSessionProfile); got != "databricks:qa" || prof != "qa" {
		t.Fatalf("exported %s=%q %s=%q, want databricks:qa and qa", router.EnvCoworker, got, providers.EnvSessionProfile, prof)
	}
	vaulttest.Put(t, vault.Env, router.EnvCoworker, "bogus:qa")
	t.Setenv(router.EnvCoworker, "")
	err := ApplyCoworker("", "", "codex", "", router.FallbackAll)
	if want := "vault env " + router.EnvCoworker + ": "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("malformed vault value: err = %v, want prefix %q", err, want)
	}
}

// ConfineLead fills an untyped lead from a non-Databricks confinement; a typed profile:model passes through untouched.
func TestConfineLeadFillsNonDatabricksLead(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{`+acmeProvider+`}`)
	cases := []struct {
		coworker, agent, provider, model string
		wantProfile, wantProvider        string
	}{
		{"openai:acme", "codex", "", "", "acme", ""},
		{"openai:acme", "codex", "openai", "", "acme", ""}, // a typed --provider means the pinned profile
		{"openai:acme", "codex", "", "acme:m", "", ""},     // a typed profile:model is never rewritten
		{"openai", "codex", "", "", "", "openai"},
		{"openai", "codex", "", "acme:m", "", ""},
		{"anthropic", "claude", "", "", "ultra", ""}, // claude reads a profile: its own CLI-login row
	}
	for _, c := range cases {
		t.Setenv(router.EnvCoworker, c.coworker)
		profile, provider := ConfineLead(c.agent, "", c.provider, c.model)
		if profile != c.wantProfile || provider != c.wantProvider {
			t.Errorf("%s %s --provider %q --model %q: ConfineLead = (%q, %q), want (%q, %q)",
				c.coworker, c.agent, c.provider, c.model, profile, provider, c.wantProfile, c.wantProvider)
		}
	}
}

// confineLine is a batch line's only fence: an off-route profile or an agent with no account is refused.
func TestConfineLineRefusesOffRouteBatchLine(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(router.EnvCoworker, "databricks:dev")
	if _, err := confineLine("claude", agent.RunOptions{Provider: "qa"}); err == nil ||
		!strings.Contains(err.Error(), "--profile qa: outside overlay databricks:dev") {
		t.Errorf("off-workspace line: err = %v, want --profile qa: outside overlay databricks:dev", err)
	}
	t.Setenv(router.EnvCoworker, "openai")
	if _, err := confineLine("copilot", agent.RunOptions{}); err == nil || !strings.Contains(err.Error(), "copilot has no openai account") {
		t.Errorf("copilot line under openai: err = %v, want copilot has no openai account", err)
	}
	got, err := confineLine("codex", agent.RunOptions{Prompt: "p"})
	reg, lerr := providers.Load()
	if err != nil || lerr != nil || got.Provider != "" || !slices.Contains(router.OwnProfiles(reg, "codex", "openai"), got.Lead) {
		t.Errorf("clean codex line under openai = lead %q provider %q, %v (%v); want codex's openai row as lead",
			got.Lead, got.Provider, err, lerr)
	}
}

// TestPickArm: fewest live wins; a tie goes to the least recent pick, then the name; an unusable workspace is skipped.
func TestPickArm(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	cases := []struct {
		name     string
		live     map[string]int
		picked   map[string]time.Time
		unusable []string
		want     string
		wantLive int
	}{
		{"fewest live", map[string]int{"dev": 3, "prod": 2, "qa": 1}, nil, nil, "qa", 1},
		{"tie: least recent pick", map[string]int{"dev": 1, "prod": 1, "qa": 1},
			map[string]time.Time{"dev": ago(time.Minute), "prod": ago(time.Hour), "qa": ago(2 * time.Minute)}, nil, "prod", 1},
		{"tie: never picked beats picked", nil, map[string]time.Time{"dev": ago(time.Hour), "prod": ago(time.Hour)}, nil, "qa", 0},
		{"tie: name order", nil, nil, nil, "dev", 0},
		{"no usable credential is skipped", map[string]int{"prod": 9, "qa": 5}, nil, []string{"dev"}, "qa", 5},
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"openai": {Name: "openai"}}}
	for _, n := range []string{"dev", "prod", "qa"} {
		reg.Providers[n] = &providers.Provider{Name: n, Preset: providers.PresetDatabricks}
	}
	origFindings, origCooling, origLive := armFindings, armCooling, armLive
	t.Cleanup(func() { armFindings, armCooling, armLive = origFindings, origCooling, origLive })
	armCooling = func(*providers.Provider, time.Time) bool { return false }
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			if err := writeArmPicks(armPicks{Schema: armPicksSchema, Picked: c.picked}); err != nil {
				t.Fatal(err)
			}
			armFindings = noCredential(c.unusable)
			armLive = func([]*providers.Provider) map[string]int { return c.live }
			got, live, skipped, err := pickArm(reg, "", "", now)
			if err != nil || got != c.want || live != c.wantLive || len(skipped) != len(c.unusable) {
				t.Fatalf("pickArm = %q, %d live, %d skipped, %v; want %q, %d live, %d skipped",
					got, live, len(skipped), err, c.want, c.wantLive, len(c.unusable))
			}
			if at := readArmPicks().Picked[got]; !at.Equal(now) {
				t.Fatalf("pick of %s stamped %v, want %v: the next tie would go to it again", got, at, now)
			}
		})
	}
	armFindings = noCredential([]string{"dev", "prod", "qa"})
	if got, _, _, err := pickArm(reg, "", "", now); err == nil {
		t.Fatalf("pickArm with no usable workspace = %q, want an error", got)
	}
}

// noCredential is an armFindings stub failing the credential check of each workspace in unusable.
func noCredential(unusable []string) func(*providers.Provider, string, string, time.Time) []agent.Finding {
	return func(p *providers.Provider, _, _ string, _ time.Time) []agent.Finding {
		if slices.Contains(unusable, p.Name) {
			return []agent.Finding{{Check: "credential", Profile: p.Name, Detail: "no stored credential for " + p.Name}}
		}
		return nil
	}
}
