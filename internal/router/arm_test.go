package router

import (
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet"
	"github.com/edotau/claude-code/internal/providers"
)

type armFinding struct{ Profile string }
type armQuery = WorkspaceQuery[armFinding]

var (
	armFindings = noCredential(nil)
	armCooling  = func(*providers.Provider, time.Time) bool { return false }
	armLive     = func([]*providers.Provider) map[string]int { return nil }
)

func pickWith(reg *providers.Registry, q armQuery, now time.Time) (string, int, []armFinding, error) {
	q.Findings, q.Cooling, q.Live = armFindings, armCooling, armLive
	return PickWorkspaceAndStamp(reg, q, now)
}

func pickArm(reg *providers.Registry, agentName, model string, now time.Time, tried ...string) (string, int, []armFinding, error) {
	return pickWith(reg, armQuery{AgentName: agentName, Model: model, Tried: tried}, now)
}

func noCredential(unusable []string) func(*providers.Provider, string, string, time.Time) []armFinding {
	return func(p *providers.Provider, _, _ string, _ time.Time) []armFinding {
		if slices.Contains(unusable, p.Name) {
			return []armFinding{{Profile: p.Name}}
		}
		return nil
	}
}

func poolFixture(t *testing.T, live map[string]int) *providers.Registry {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	origFindings, origCooling, origLive := armFindings, armCooling, armLive
	t.Cleanup(func() { armFindings, armCooling, armLive = origFindings, origCooling, origLive })
	armFindings = noCredential(nil)
	armCooling = func(*providers.Provider, time.Time) bool { return false }
	armLive = func([]*providers.Provider) map[string]int { return live }
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	for _, n := range []string{"dev", "prod", "qa"} {
		reg.Providers[n] = &providers.Provider{Name: n, Preset: providers.PresetDatabricks}
	}
	return reg
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
			checkReadOnlyRank(t, reg, now, c.want)
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

// PickRowsAndStamp with Extra rotates least-recent over the workspaces plus the extra row, skipping unusable ones.
func TestPickRowsRotation(t *testing.T) {
	ultra := &providers.Provider{Name: "ultra", Provider: "anthropic", Auth: providers.Auth{Type: providers.AuthCLI}}
	for _, c := range []struct {
		name     string
		unusable []string
		want     []string
	}{
		{"four rows, each once per cycle", nil, []string{"dev", "prod", "qa", "ultra", "dev", "prod", "qa", "ultra"}},
		{"held and credential-less rows skipped", []string{"prod", "qa"}, []string{"dev", "ultra", "dev", "ultra"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			reg := poolFixture(t, map[string]int{"dev": 5}) // load is ignored: Live is the caller's, nil here
			reg.Providers["anthropic"] = &providers.Provider{Name: "anthropic", Provider: "anthropic", Auth: providers.Auth{Type: "key"}}
			reg.Providers["ultra"] = ultra
			if err := writeArmPicks(armPicks{Schema: armPicksSchema, Picked: map[string]time.Time{"dev": time.Now()}}); err != nil {
				t.Fatal(err)
			}
			q := armQuery{Extra: []*providers.Provider{ultra}, Key: RowRotationKey("claude"), Findings: noCredential(c.unusable),
				Cooling: armCooling, Live: func([]*providers.Provider) map[string]int { return nil }}
			now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
			var got []string
			for i := range c.want {
				rows, skipped, err := PickRowsAndStamp(reg, q, now.Add(time.Duration(i)*time.Second))
				if err != nil || len(skipped) != len(c.unusable) || len(rows) != 4-len(c.unusable) {
					t.Fatalf("pick %d = %v, %d skipped, %v", i, rows, len(skipped), err)
				}
				got = append(got, rows[0])
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("picks = %v, want %v", got, c.want)
			}
		})
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

// A batch's picks count as load before any marker exists; concurrent picks land on distinct workspaces.
func TestPickWithPendingAndConcurrent(t *testing.T) {
	reg := poolFixture(t, nil)
	pending := map[string]int{}
	seen := map[string]int{}
	for range 6 {
		name, _, _, err := pickWith(reg, armQuery{Pending: pending}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		seen[name]++
	}
	if len(seen) != 3 || seen["dev"] != 2 || seen["prod"] != 2 || seen["qa"] != 2 {
		t.Fatalf("six batch picks = %v, want two on each workspace", seen)
	}
	var wg sync.WaitGroup
	out := make(chan string, 3)
	for range 3 {
		wg.Go(func() {
			name, _, _, _ := pickWith(reg, armQuery{}, time.Now())
			out <- name
		})
	}
	wg.Wait()
	close(out)
	distinct := map[string]bool{}
	for n := range out {
		distinct[n] = true
	}
	if len(distinct) != 3 {
		t.Fatalf("three concurrent picks chose %v, want three distinct workspaces", distinct)
	}
	spread := make(chan string, 6)
	for range 6 {
		wg.Go(func() {
			name, _, _, _ := pickWith(reg, armQuery{}, time.Now())
			spread <- name
		})
	}
	wg.Wait()
	close(spread)
	per := map[string]int{}
	for n := range spread {
		per[n]++
	}
	if per["dev"] != 2 || per["prod"] != 2 || per["qa"] != 2 {
		t.Fatalf("six concurrent picks = %v, want two on each workspace", per)
	}
}

func checkReadOnlyRank(t *testing.T, reg *providers.Registry, now time.Time, want string) {
	t.Helper()
	before := readArmPicks()
	q := armQuery{Findings: armFindings, Cooling: armCooling, Live: armLive, Pending: map[string]int{}}
	ranked, _, err := RankWorkspaces(reg, q, now)
	if err != nil || len(ranked) == 0 || ranked[0] != want || !maps.Equal(before.Picked, readArmPicks().Picked) || len(q.Pending) != 0 {
		t.Fatalf("read-only rank = %v, %v; want %s without writes", ranked, err, want)
	}
}
