package router

import (
	"cmp"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	dbxserving "github.com/edotau/claude-code/internal/databricks/serving"

	_ "github.com/edotau/claude-code/internal/nonet"
	"github.com/edotau/claude-code/internal/paths"
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

func setArmHost(t testing.TB, host string) {
	t.Helper()
	orig := armHost
	t.Cleanup(func() { armHost = orig })
	armHost = func() string { return host }
}

func poolFixture(t *testing.T, live map[string]int) *providers.Registry {
	t.Helper()
	setArmHost(t, "devhost")
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

// Equal load favors the least recent pick, then the host salt; unusable workspaces are skipped.
func TestPickArm(t *testing.T) {
	setArmHost(t, "devhost")
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
		{"tie: host order", nil, nil, nil, "prod", 0},
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
	setArmHost(t, "devhost")
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
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, want := range []string{"prod", "qa", "dev"} {
		got, _, _, err := pickArm(reg, "", "", now, tried...)
		if err != nil || got != want {
			t.Fatalf("pickArm outside %v = %q, %v; want %q", tried, got, err, want)
		}
		tried = append(tried, got)
	}
	if got, _, _, err := pickArm(reg, "", "", now, tried...); err == nil {
		t.Fatalf("pickArm with every workspace tried = %q, want an error", got)
	}
}

// BenchmarkPickArmExcluding is the hot path a 429 adds: one pick outside the workspaces already tried.
func BenchmarkPickArmExcluding(b *testing.B) {
	setArmHost(b, "devhost")
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

// Separate launches' fresh picks count as load before their children register, so a burst spreads.
func TestPickArmBurstSpreads(t *testing.T) {
	reg := poolFixture(t, map[string]int{"prod": 1})
	now := time.Now()
	seen := map[string]bool{}
	for range 3 {
		got, _, _, err := pickArm(reg, "", "", now)
		if err != nil {
			t.Fatal(err)
		}
		seen[got] = true
	}
	if len(seen) != 3 {
		t.Fatalf("three burst picks chose %v, want three distinct workspaces", seen)
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

func rankIs(ranked []string, err error, at map[int]string, n int) bool {
	if err != nil || len(ranked) != n {
		return false
	}
	for i, name := range at {
		if ranked[i] != name {
			return false
		}
	}
	return true
}

// tieOrder is names in this host's cold-start tie-break order.
func tieOrder(names ...string) []string {
	host := armHost()
	return slices.SortedFunc(slices.Values(names), func(a, b string) int {
		return cmp.Compare(hostTie(host, a), hostTie(host, b))
	})
}

// markLoginDead writes the recorded dead-login state of profile name.
func markLoginDead(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(paths.StateDir(), "cache", "login-state")
	mark := `{"schema":1,"state":"needs_login","since":"2026-01-01T00:00:00Z","until":"2999-01-01T00:00:00Z"}`
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(mark), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRankWorkspacesPreferFreshAndMultiProvider(t *testing.T) {
	setArmHost(t, "devhost")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"ws-a": {Name: "ws-a", Provider: "databricks", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: providers.AuthOAuth}, Host: "https://a.example.com"},
		"ws-b": {Name: "ws-b", Provider: "databricks", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: providers.AuthOAuth}, Host: "https://b.example.com"},
		"ws-c": {Name: "ws-c", Provider: "databricks", Preset: providers.PresetDatabricks, Host: "https://c.example.com"},
		"ws-d": {Name: "ws-d", Provider: "databricks", Preset: providers.PresetDatabricks, Host: "https://d.example.com"},
		"ws-e": {Name: "ws-e", Provider: "databricks", Preset: providers.PresetDatabricks, Host: "https://e.example.com"},
		"gh-a": {Name: "gh-a", Provider: "github"},
		"gh-b": {Name: "gh-b", Provider: "github"},
	}}
	if err := dbxserving.WarmServed("ws-c", []providers.ModelInfo{{ID: "databricks-claude-opus-5-5", Ready: "READY"}}, now); err != nil {
		t.Fatal(err)
	}
	baseQ := func(prov string, cool map[string]bool, preferFresh bool) WorkspaceQuery[struct{}] {
		return WorkspaceQuery[struct{}]{
			Provider: prov, PreferFresh: preferFresh,
			Findings: func(*providers.Provider, string, string, time.Time) []struct{} { return nil },
			Unserved: func(string, []struct{}) error { return nil },
			Cooling:  func(p *providers.Provider, _ time.Time) bool { return cool[p.Name] },
			Live:     func([]*providers.Provider) map[string]int { return map[string]int{} },
		}
	}
	// D6: fresh ws-c ranks ahead of stale ws-a..ws-e when PreferFresh is set.
	ranked, _, err := RankWorkspaces(reg, baseQ("databricks", nil, true), now)
	if !rankIs(ranked, err, map[int]string{0: "ws-c"}, 5) {
		t.Fatalf("preferFresh rank = %v, %v; want 5 workspaces with ws-c first", ranked, err)
	}
	// Cooling still beats freshness: if ws-c is cooling, a stale workspace takes over.
	ranked, _, err = RankWorkspaces(reg, baseQ("databricks", map[string]bool{"ws-c": true}, true), now)
	if !rankIs(ranked, err, map[int]string{4: "ws-c"}, 5) {
		t.Fatalf("cooling vs fresh rank = %v, %v; want ws-c last", ranked, err)
	}
	// A recorded dead user login with no service principal ranks last; one the user identity covers is not demoted.
	markLoginDead(t, "ws-a")
	ranked, _, err = RankWorkspaces(reg, baseQ("databricks", nil, false), now)
	if !rankIs(ranked, err, map[int]string{4: "ws-a"}, 5) {
		t.Fatalf("dead-login rank = %v, %v; want ws-a last", ranked, err)
	}
	reg.Providers["ws-a"].Auth.Type = providers.AuthNone // no oauth login to be dead: back in place
	if ranked, _, err = RankWorkspaces(reg, baseQ("databricks", nil, false), now); err != nil || !slices.Equal(ranked, tieOrder(ranked...)) {
		t.Fatalf("no-login rank = %v, %v; want the host tie order", ranked, err)
	}
	ghRanked, _, err := RankWorkspaces(reg, baseQ("github", map[string]bool{"gh-a": true}, false), now)
	if !rankIs(ghRanked, err, map[int]string{0: "gh-b", 1: "gh-a"}, 2) {
		t.Fatalf("github rank = %v, %v; want [gh-b gh-a]", ghRanked, err)
	}
}

func TestRankWorkspacesFreshBeforeLoadAndHostTie(t *testing.T) {
	setArmHost(t, "devhost")
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	for _, n := range []string{"dev", "qa", "prod"} {
		reg.Providers[n] = &providers.Provider{Name: n, Provider: "databricks", Preset: providers.PresetDatabricks, Host: "https://" + n + ".example.com"}
	}
	if err := dbxserving.WarmServed("dev", []providers.ModelInfo{{ID: "databricks-claude-opus-5-5", Ready: "READY"}}, now); err != nil {
		t.Fatal(err)
	}
	q := armQuery{Findings: armFindings, Unserved: func(string, []armFinding) error { return nil }, Cooling: armCooling, PreferFresh: true,
		Live: func([]*providers.Provider) map[string]int { return map[string]int{"dev": 1} }}
	// PreferFresh: a busy fresh dev beats the idle stale qa and prod, so auto gets a listing to preflight.
	if ranked, _, err := RankWorkspaces(reg, q, now); !rankIs(ranked, err, map[int]string{0: "dev"}, 3) {
		t.Fatalf("PreferFresh rank = %v, %v; want busy fresh dev first", ranked, err)
	}
	// Without PreferFresh load decides: the busy dev ranks last.
	q.PreferFresh = false
	if ranked, _, err := RankWorkspaces(reg, q, now); !rankIs(ranked, err, map[int]string{2: "dev"}, 3) {
		t.Fatalf("load rank = %v, %v; want busy dev last", ranked, err)
	}
	// Equal load, no stamps: two hosts prefer different first workspaces.
	q.Live = armLive
	for _, row := range []struct {
		host string
		want []string
	}{
		{"devhost", []string{"prod", "qa", "dev"}},
		{"otherhost", []string{"dev", "qa", "prod"}},
	} {
		t.Run(row.host, func(t *testing.T) {
			setArmHost(t, row.host)
			if ranked, _, err := RankWorkspaces(reg, q, now); err != nil || !slices.Equal(ranked, row.want) {
				t.Fatalf("rank = %v, %v; want %v", ranked, err, row.want)
			}
		})
	}
}
