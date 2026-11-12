package ask

import (
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

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
