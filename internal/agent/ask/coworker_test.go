package ask

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// confine sets this process's confinement to provider[:profile] as ApplyCoworker exports it; "" is none.
func confine(t *testing.T, v string) {
	provider, profile, _ := strings.Cut(v, ":")
	t.Setenv(agentconfig.OverlayVar, agentconfig.InlineOverlay(provider, profile))
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
		confine(t, c.coworker)
		profile, provider := router.ConfineLead(c.agent, "", c.provider, c.model)
		if profile != c.wantProfile || provider != c.wantProvider {
			t.Errorf("%s %s --provider %q --model %q: ConfineLead = (%q, %q), want (%q, %q)",
				c.coworker, c.agent, c.provider, c.model, profile, provider, c.wantProfile, c.wantProvider)
		}
	}
}

// confineLine is a batch line's only fence: an off-route profile or an agent with no account is refused.
func TestConfineLineRefusesOffRouteBatchLine(t *testing.T) {
	vaulttest.Use(t)
	confine(t, "databricks:dev")
	if _, err := confineLine("claude", agent.RunOptions{Provider: "qa"}); err == nil ||
		!strings.Contains(err.Error(), "--profile qa: outside overlay databricks:dev") {
		t.Errorf("off-workspace line: err = %v, want --profile qa: outside overlay databricks:dev", err)
	}
	confine(t, "openai")
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

// pickArm outside tried never returns a tried workspace, and none left is an error.
func TestPickArmExcludesTried(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	for _, n := range []string{"dev", "prod", "qa"} {
		reg.Providers[n] = &providers.Provider{Name: n, Preset: providers.PresetDatabricks}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	origFindings, origCooling, origLive := armFindings, armCooling, armLive
	t.Cleanup(func() { armFindings, armCooling, armLive = origFindings, origCooling, origLive })
	armFindings = noCredential(nil)
	armCooling = func(*providers.Provider, time.Time) bool { return false }
	armLive = func([]*providers.Provider) map[string]int { return nil }
	var tried []string
	for _, want := range []string{"dev", "prod", "qa"} {
		got, _, _, err := pickArm(reg, "", "", time.Now(), tried...)
		if err != nil || got != want {
			t.Fatalf("pickArm outside %v = %q, %v; want %q", tried, got, err, want)
		}
		tried = append(tried, got)
	}
	if got, _, _, err := pickArm(reg, "", "", time.Now(), tried...); err == nil {
		t.Fatalf("pickArm with every workspace tried = %q, want an error", got)
	}
}

// BenchmarkPickArmExcluding is the hot path a 429 adds: one pick outside the workspaces already tried.
func BenchmarkPickArmExcluding(b *testing.B) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	for _, n := range []string{"dev", "prod", "qa"} {
		reg.Providers[n] = &providers.Provider{Name: n, Preset: providers.PresetDatabricks}
	}
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	origFindings, origCooling, origLive := armFindings, armCooling, armLive
	b.Cleanup(func() { armFindings, armCooling, armLive = origFindings, origCooling, origLive })
	armFindings = noCredential(nil)
	armCooling = func(*providers.Provider, time.Time) bool { return false }
	armLive = func([]*providers.Provider) map[string]int { return nil }
	for b.Loop() {
		if _, _, _, err := pickArm(reg, "", "", time.Now(), "dev"); err != nil {
			b.Fatal(err)
		}
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
