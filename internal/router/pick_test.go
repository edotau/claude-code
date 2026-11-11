package router

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/providers"
)

func testTable(t *testing.T) *RouteTable {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // scratch vault: never the real pins or logins
	// a bare table.json load carries no registry, so set the defaults.json-shaped opencode free-tier
	// policy explicitly: zen-free may serve sonnet/haiku, same as production's merged registry.
	reg := &providers.Registry{Routing: providers.Routing{Opencode: providers.OpencodeRouting{FreeTiers: &[]string{"sonnet", "haiku"}}}}
	tb, err := LoadFrom(reg, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func allConfigured(string, Cell) error { return nil }

var now0 = time.Date(2026, 10, 25, 2, 6, 0, 0, time.Local)

// assertEveryCellResolves checks each agent x alias cell has a model, account and billing, and AliasOf round-trips it.
func assertEveryCellResolves(t *testing.T, tb *RouteTable) {
	t.Helper()
	for _, r := range TableAgents {
		for _, alias := range Aliases {
			c, ok := tb.Cell(r, alias)
			if !ok || c.Model == "" || c.Account == "" || tb.Billing[c.Account] == "" {
				t.Fatalf("%s/%s: cell %+v ok=%v billing=%q", r, alias, c, ok, tb.Billing[c.Account])
			}
			if got := tb.AliasOf(c.Model); got != alias && !sharedStrongerAlias(r, alias, got) {
				t.Errorf("AliasOf(%s) = %q, want %q", c.Model, got, alias)
			}
		}
	}
}

// sharedStrongerAlias: codex and opencode serve one id at both opus and sonnet, so the strongest alias claims it.
func sharedStrongerAlias(agent, alias, got string) bool {
	return (agent == "codex" || agent == "opencode") && alias == "sonnet" && got == "opus"
}

func TestEveryCellResolvesAndTierOfRoundTrips(t *testing.T) {
	tb := testTable(t)
	assertEveryCellResolves(t, tb)
	if a := tb.Rows["copilot"]["sonnet"].Account; a != "github·work-user" {
		t.Errorf("copilot account = %q, want the pinned user only", a)
	}
	if got := tb.AliasOf("claude-opus-5-5[1m]"); got != "opus" {
		t.Errorf("AliasOf(claude id) = %q", got)
	}
}

func TestWeightValidation(t *testing.T) {
	for name, w := range map[string]map[string]float64{
		"sum":     {"price": 0.5, "headroom": 0.6},
		"missing": {"price": 1},
		"extra":   {"price": 0.5, "headroom": 0.5, "latency": 0},
		"range":   {"price": 1.5, "headroom": -0.5},
	} {
		if _, err := LoadFrom(&providers.Registry{Routing: providers.Routing{Weights: w}}, "u"); err == nil {
			t.Errorf("%s: weights %v accepted", name, w)
		}
	}
}

func TestParseReset(t *testing.T) {
	cases := []struct {
		text string
		want time.Time
	}{
		{"You've hit your usage limit. Try again at 2:17 AM.", time.Date(2026, 10, 25, 2, 17, 0, 0, time.Local)},
		{"usage limit, try again at 1:00 am", time.Date(2026, 10, 26, 1, 0, 0, 0, time.Local)},
		{"rate limited: try again in 37 minutes", now0.Add(37 * time.Minute)},
		{"402 Payment Required: you have exceeded your monthly quota", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
		{"quota exhausted until 2026-11-01", time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)},
	}
	for _, c := range cases {
		got, ok := ParseReset(c.text, now0)
		if !ok || !got.Equal(c.want) {
			t.Errorf("ParseReset(%q) = %v %v, want %v", c.text, got, ok, c.want)
		}
	}
}

func TestParseResetZones(t *testing.T) {
	if got, _ := ParseReset("resets 2026-10-26T09:00:00Z", now0); !got.Equal(time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("a Z timestamp must keep UTC, got %v", got)
	}
	ny, _ := time.LoadLocation("America/New_York")
	if got := ResetFor("boom", BillCredits, Entry{}, time.Date(2026, 10, 31, 21, 0, 0, 0, ny)); !got.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Oct 31 21:00 NY is Nov 1 UTC: next month boundary = %v", got)
	}
}

func TestParseResetNoMatchAndQuota(t *testing.T) {
	if _, ok := ParseReset("connection reset", now0); ok {
		t.Error("no reset in a non-quota text")
	}
	for _, q := range []string{"HTTP 402", "insufficient_quota", "You've hit your usage limit", "limit: 0"} {
		if !providers.IsQuota(q) {
			t.Errorf("IsQuota(%q) = false", q)
		}
	}
	if providers.IsQuota("429 too many requests") || providers.IsQuota("line 4021") {
		t.Error("a plain rate limit or a bare number is not a quota failure")
	}
	if got := ResetFor("boom", BillCredits, Entry{}, now0); !got.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("credits default reset = %v", got)
	}
}

func TestStatusThresholds(t *testing.T) {
	seen := now0.Add(-time.Minute)
	for _, c := range []struct {
		e    Entry
		want string
	}{
		{Entry{}, StateUnknown},
		{Entry{UsedPct: 50, Seen: seen}, stateOK},
		{Entry{UsedPct: 90, Seen: seen}, StateLow},
		{Entry{UsedPct: 100, Seen: seen, ResetsAt: now0.Add(time.Hour)}, StateExhausted},
		{Entry{UsedPct: 100, Seen: seen, ResetsAt: now0.Add(-time.Hour)}, stateOK},
		{Entry{UsedPct: 5, Seen: seen, ExhaustedUntil: now0.Add(time.Hour)}, StateExhausted},
		{Entry{UsedPct: 5, Seen: seen, ExhaustedUntil: now0.Add(-time.Hour)}, stateOK},
	} {
		if got := c.e.Status(now0); got != c.want {
			t.Errorf("Status(%+v) = %s, want %s", c.e, got, c.want)
		}
	}
}

func pick(tb *RouteTable, alias, want string, st State, mode string) PickResult {
	return tb.Pick(context.Background(), alias, want, st, PickOpts{Fallback: mode, Configured: allConfigured, Now: func() time.Time { return now0 }})
}

// pickRepo is pick with the work-repo answer pinned, so the opencode sub-pick's free gate is deterministic.
func pickRepo(tb *RouteTable, alias string, st State, work bool) PickResult {
	if len(tb.OpencodeLadder) == 0 {
		tb.OpencodeLadder = []string{"opencode-go", "github-copilot", "openai"} // defaults.json order; LoadFrom(nil) has none
	}
	return tb.Pick(context.Background(), alias, "", st, PickOpts{Fallback: FallbackAgents, Configured: allConfigured,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return work }})
}

func runners(r PickResult) string {
	var s []string
	for _, c := range r.Candidates {
		s = append(s, c.Agent)
	}
	return strings.Join(s, ",")
}

func TestPickGatesBeatScore(t *testing.T) {
	tb := testTable(t)
	st := State{"github·work-user": {UsedPct: 0, Seen: now0, ExhaustedUntil: time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local), Reason: "402"}}
	r := pick(tb, "sonnet", "copilot", st, FallbackAll)
	if strings.Contains(runners(r), "copilot") {
		t.Fatalf("exhausted copilot picked: %s", runners(r))
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Gate != GateExhausted || !strings.Contains(r.Skipped[0].Reason, "github·work-user exhausted until 2026-11-01") {
		t.Errorf("skips = %+v", r.Skipped)
	}
	if last := r.Candidates[len(r.Candidates)-1]; last.Agent != "claude" || last.Model != "claude-sonnet-5-5" {
		t.Errorf("claude dispatch = %+v, want sonnet alias kept", last)
	}
}

func TestPickDemotesQuotaDeadModelBehindLiveCandidates(t *testing.T) {
	tb := testTable(t)
	baseline := pick(tb, "sonnet", "codex", State{}, FallbackAgents)
	if len(baseline.Candidates) == 0 || baseline.Candidates[0].Agent != "codex" {
		t.Fatalf("baseline candidates = %+v, want codex first", baseline.Candidates)
	}
	first := baseline.Candidates[0]
	dead := State{first.Account: {ModelDead: map[string]time.Time{ModelKey(first.Model): now0.Add(time.Hour)}}}
	got := pick(tb, "sonnet", "codex", dead, FallbackAgents)
	if len(got.Candidates) == 0 || got.Candidates[len(got.Candidates)-1].Agent != "codex" {
		t.Errorf("quota-dead codex candidates = %+v, want codex last", got.Candidates)
	}
	var quotaSkip int
	for _, skipped := range got.Skipped {
		if skipped.Gate == GateExhausted && strings.Contains(skipped.Reason, "quota-denied") &&
			strings.Contains(skipped.Reason, "tried last") {
			quotaSkip++
		}
	}
	if quotaSkip != 1 {
		t.Errorf("quota-dead skips = %+v, want one tried-last quota skip", got.Skipped)
	}
	expired := State{first.Account: {ModelDead: map[string]time.Time{ModelKey(first.Model): now0.Add(-time.Hour)}}}
	afterExpiry := pick(tb, "sonnet", "codex", expired, FallbackAgents)
	if runners(afterExpiry) != runners(baseline) {
		t.Errorf("expired-mark order = %s, want baseline order %s", runners(afterExpiry), runners(baseline))
	}
}

func TestPickUnpinnedCopilotAndFallbackModes(t *testing.T) {
	tb := testTable(t)
	unpinned, _ := LoadFrom(nil, "")
	r := pick(unpinned, "opus", "copilot", State{}, FallbackAgents)
	if r.Skipped[0].Gate != gateAccount || strings.Contains(runners(r), "claude") {
		t.Errorf("unpinned copilot must drop at the account gate, vendors mode has no claude: %+v %s", r.Skipped, runners(r))
	}
	if got := runners(pick(tb, "opus", "copilot", State{}, FallbackNone)); got != "copilot" {
		t.Errorf("fallback none = %s", got)
	}
	if got := runners(pick(tb, "opus", "claude", State{}, FallbackAll)); got != "claude" {
		t.Errorf("claude never falls back: %s", got)
	}
}

func TestPickTwoDimOrderingAndLowLast(t *testing.T) {
	tb := testTable(t)
	st := State{
		"openai·chatgpt":     {UsedPct: 10, Seen: now0},
		"google·antigravity": {UsedPct: 60, Seen: now0},
		"opencode·go":        {UsedPct: 20, Seen: now0},
	}
	r := pickRepo(tb, "sonnet", st, true) // work repo: no free dispatch, so opencode lands on its Go account
	if got := runners(r); got != "codex,opencode,copilot,gemini" {
		t.Errorf("order = %s (scores %+v)", got, r.Candidates)
	}
	c := r.Candidates[0]
	if c.Scores["price"] != 0.9 || c.Scores["headroom"] != 0.9 || c.Total != 0.9 {
		t.Errorf("codex scores = %+v total %v", c.Scores, c.Total)
	}
	st["openai·chatgpt"] = Entry{UsedPct: 95, Seen: now0}
	if got := runners(pickRepo(tb, "sonnet", st, true)); !strings.HasSuffix(got, ",codex") {
		t.Errorf("low codex must sort after every non-low agent: %s", got)
	}
	out, _ := json.Marshal(r)
	for _, k := range []string{`"weights"`, `"scores"`, `"total"`} {
		if !strings.Contains(string(out), k) {
			t.Errorf("JSON lacks %s: %s", k, out)
		}
	}
}

// TestPriceDimCheaperCreditsOutranksAtEqualHeadroom checks that between two credits-billed candidates
// with equal headroom (both unseen accounts), the cheaper blended price scores higher and so outranks
// the pricier one in the composite total.
func TestPriceDimCheaperCreditsOutranksAtEqualHeadroom(t *testing.T) {
	tb := testTable(t)
	cheap := Candidate{Agent: "codex", Billing: BillCredits, Account: "openai·chatgpt", Price: Price{In: 1, Out: 1}}
	pricey := Candidate{Agent: "gemini", Billing: BillCredits, Account: "google·antigravity", Price: Price{In: 4, Out: 4}}
	st := State{} // both accounts unseen: headroom ties at 0.5 for each
	scored := tb.score([]Candidate{cheap, pricey}, st, time.Time{})
	a, b := scored[0], scored[1]
	if a.Scores["headroom"] != b.Scores["headroom"] {
		t.Fatalf("precondition: headroom must tie, got %v vs %v", a.Scores["headroom"], b.Scores["headroom"])
	}
	if a.Scores["price"] <= b.Scores["price"] {
		t.Errorf("cheap price score = %v, want it above the pricier candidate's %v", a.Scores["price"], b.Scores["price"])
	}
	if a.Total <= b.Total {
		t.Errorf("cheap total = %v, want it to outrank the pricier candidate's %v", a.Total, b.Total)
	}
}

// TestPickOpencodeGithubCopilotOverrideHeadroomUnknown checks an opencode override to github-copilot bills
// opencode·github-copilot (not the pinned gh user's github·<user> key) and, with no remembered state for
// that never-seen account, reads as StateUnknown with headroom 0.5 — never conflated with the copilot row's
// own account state.
func TestPickOpencodeGithubCopilotOverrideHeadroomUnknown(t *testing.T) {
	reg := &providers.Registry{
		Routing: providers.Routing{Cells: map[string]map[string]providers.RouteCell{"opencode": {"sonnet": {Model: "github-copilot/kimi-k3"}}}},
	}
	tb, err := LoadFrom(reg, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	r := pick(tb, "sonnet", "opencode", State{}, FallbackNone)
	if len(r.Candidates) != 1 {
		t.Fatalf("candidates = %+v, want exactly the wanted opencode dispatch", r.Candidates)
	}
	c := r.Candidates[0]
	if c.Account != "opencode·github-copilot" {
		t.Errorf("account = %q, want opencode's own copilot login, not the pinned gh user's", c.Account)
	}
	if c.State != StateUnknown {
		t.Errorf("state = %q, want unknown (never-seen account)", c.State)
	}
	if c.Scores["headroom"] != 0.5 {
		t.Errorf("headroom = %v, want 0.5 for an unseen account", c.Scores["headroom"])
	}
	// A reading on the pinned gh user's own copilot account must not leak into the derived one.
	st := State{"github·work-user": {UsedPct: 90, Seen: now0}}
	r = pick(tb, "sonnet", "opencode", st, FallbackNone)
	if got := r.Candidates[0].Scores["headroom"]; got != 0.5 {
		t.Errorf("headroom = %v, want 0.5 still — github·work-user's state must not apply to opencode·github-copilot", got)
	}
}

func TestStateRoundTripScopedToConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := Record("openai·chatgpt", 40, "5h", now0.Add(time.Hour), now0); err != nil {
		t.Fatal(err)
	}
	if err := MarkExhausted("github·u", now0.Add(time.Hour), now0, "402"); err != nil {
		t.Fatal(err)
	}
	if err := MarkExhausted("github·u", now0.Add(time.Minute), now0, "short"); err != nil {
		t.Fatal(err)
	}
	s := LoadState()
	if !s["github·u"].ExhaustedUntil.Equal(now0.Add(time.Hour)) {
		t.Errorf("a shorter concurrent deadline replaced a longer one: %v", s["github·u"].ExhaustedUntil)
	}
	if s["openai·chatgpt"].UsedPct != 40 || s["github·u"].Status(now0) != StateExhausted {
		t.Errorf("state = %+v", s)
	}
	if !strings.HasPrefix(StatePath(), dir) {
		t.Errorf("StatePath = %s", StatePath())
	}
}

// TestMain runs in the hermetic host (hosttest): a test must never read the developer's vault pins.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// A refresh that records nothing stamps the account checked: the next pick inside StaleAfter skips the fetch and the
// account still reads unknown; past the TTL the pick fetches again, each account once.
func TestPickBacksOffAfterNoDataRefresh(t *testing.T) {
	tb := testTable(t)
	at := now0
	var mu sync.Mutex
	var calls []string
	o := PickOpts{Fallback: FallbackAgents, Configured: allConfigured, Now: func() time.Time { return at },
		WorkRepo: func() bool { return false },
		Refresh: func(_ context.Context, account string) bool {
			mu.Lock()
			calls = append(calls, account)
			mu.Unlock()
			if err := MarkChecked(account, "unlimited", at); err != nil {
				t.Error(err)
			}
			return false
		}}
	pick := func() []string {
		calls = nil
		tb.Pick(context.Background(), "sonnet", "", LoadState(), o)
		slices.Sort(calls)
		return calls
	}
	first := pick()
	if len(first) == 0 || len(slices.Compact(slices.Clone(first))) != len(first) {
		t.Fatalf("first pick refreshed %v, want each stale account exactly once", first)
	}
	if again := pick(); len(again) != 0 {
		t.Errorf("pick inside StaleAfter refreshed %v, want none (checked stamp)", again)
	}
	for _, account := range first {
		if e := LoadState()[account]; e.Status(at) != StateUnknown || e.CheckNote != "unlimited" {
			t.Errorf("%s = %+v, want unknown with the check note", account, e)
		}
	}
	at = at.Add(StaleAfter + time.Minute)
	if after := pick(); !slices.Equal(after, first) {
		t.Errorf("pick past StaleAfter refreshed %v, want %v", after, first)
	}
}

// A refresh that writes nothing (lock busy, caller's deadline) still runs once per account per pick, never again serially.
func TestPickRefreshesEachAccountOnce(t *testing.T) {
	tb := testTable(t)
	var mu sync.Mutex
	seen := map[string]int{}
	tb.Pick(context.Background(), "sonnet", "", State{}, PickOpts{Fallback: FallbackAgents, Configured: allConfigured,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false },
		Refresh: func(_ context.Context, account string) bool {
			mu.Lock()
			seen[account]++
			mu.Unlock()
			return false
		}})
	for account, n := range seen {
		if n != 1 {
			t.Errorf("%s refreshed %d times in one pick, want 1", account, n)
		}
	}
	if len(seen) == 0 {
		t.Error("no account refreshed")
	}
}

// The collecting pass must not advance the Discovery rotation; the real pick keeps the pin.
func TestDryPickDropsDiscoveryPin(t *testing.T) {
	if got := (PickOpts{dry: true}).modelPin(DiscoveryModel); got != "" {
		t.Errorf("dry modelPin(discovery) = %q, want \"\"", got)
	}
	if got := (PickOpts{}).modelPin(DiscoveryModel); got != DiscoveryModel {
		t.Errorf("real modelPin(discovery) = %q, want the pin", got)
	}
}

func TestPickSkipTextForDeactivatedAgent(t *testing.T) {
	tb := testTable(t)
	r := tb.Pick(context.Background(), "sonnet", "codex", State{}, PickOpts{Fallback: FallbackAll, Configured: allConfigured,
		Now: func() time.Time { return now0 }, Pins: map[string]PinSpec{"codex": {Off: true}}})
	if !slices.Contains(r.Skipped, Skip{"codex", GateOff, "deactivated (no profile set)"}) {
		t.Errorf("skips = %+v, want codex deactivated (no profile set)", r.Skipped)
	}
}

// TestArmPickWantedClaudeFallsThrough: inside the route a wanted claude leads tier 1, the other agents follow on
// databricks·dev, then the first-party logins in ladder order (its own subscription last, under all and agents).
func TestArmPickWantedClaudeFallsThrough(t *testing.T) {
	wantTier2 := []string{"copilot/github·personal", "codex/openai·chatgpt", "gemini/google·antigravity",
		"opencode/opencode·go", "claude/anthropic·ultra"}
	for _, fb := range []string{FallbackAll, FallbackAgents} {
		tier1, tier2 := armTiers(armPick(t, "claude", fb))
		if len(tier1) != 5 || tier1[0] != "claude" {
			t.Errorf("%s: tier 1 = %v, want claude first then the other four agents on databricks·dev", fb, tier1)
		}
		if !slices.Equal(tier2, wantTier2) {
			t.Errorf("%s: tier 2 = %v, want %v", fb, tier2, wantTier2)
		}
	}
	if c := armPick(t, "claude", FallbackNone).Candidates; len(c) != 1 || c[0].Account != "databricks·dev" {
		t.Errorf("fallback none = %+v, want claude on databricks·dev only", c)
	}
	calls := map[string]int{}
	(&RouteTable{}).gatherCandidates("claude", PickOpts{Fallback: FallbackAll}, func(a string) []Candidate {
		calls[a]++
		return nil
	})
	if calls["claude"] != 1 || calls["codex"] != 1 {
		t.Errorf("gate calls = %v, want claude once (no claudeLast pass) and each other agent once", calls)
	}
}

// TestIsWorkRepoMarkers exercises the gitRemoteURLs seam directly: multiple remotes, no remotes, and a
// seam error (fail-closed).
func TestIsWorkRepoMarkers(t *testing.T) {
	orig := gitRemoteURLs
	t.Cleanup(func() { gitRemoteURLs = orig })
	origMarkers := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return []string{"git.corp.example", "corp-widgets"}, nil } // test-only stand-in markers
	t.Cleanup(func() { workRepoMarkers = origMarkers })
	cases := []struct {
		name string
		urls []string
		err  error
		want bool
	}{
		{"bitbucket corp", []string{"ssh://git.corp.example:7999/digitalbtd/x.git"}, nil, true},
		{"github corp-widgets org", []string{"https://github.com/corp-widgets/tool.git"}, nil, true},
		{"personal github", []string{"https://github.com/tester/claude-code.git"}, nil, false},
		{"no remotes", nil, nil, false},
		{"personal origin, corp upstream (any url in the list is enough)", []string{
			"https://github.com/tester/claude-code.git", "https://git.corp.example/digitalbtd/x.git",
		}, nil, true},
		{"seam error fails closed", nil, errors.New("git: corrupt config"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gitRemoteURLs = func(string) ([]string, error) { return c.urls, c.err }
			dir := t.TempDir() // a real repo, so rev-parse --git-dir succeeds and the seam stub above is reached
			routeRunGit(t, dir, "init", "-q")
			if got := IsWorkRepo(dir); got != c.want {
				t.Errorf("IsWorkRepo() = %v, want %v (urls=%v, err=%v)", got, c.want, c.urls, c.err)
			}
		})
	}
}

// TestIsWorkRepoMarkersLoadErrorFailsClosed checks a workRepoMarkers load error (the registry itself,
// not the git seam) makes IsWorkRepo report true — never fail open on the free gate.
func TestIsWorkRepoMarkersLoadErrorFailsClosed(t *testing.T) {
	origMarkers := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return nil, errors.New("vault: locked") }
	t.Cleanup(func() { workRepoMarkers = origMarkers })
	dir := t.TempDir()
	routeRunGit(t, dir, "init", "-q")
	if !IsWorkRepo(dir) {
		t.Error("a markers load error must fail closed (work), not open")
	}
}

// TestIsWorkRepoEmptyMarkersIsPersonal checks no markers configured (and no load error) reports personal
// with no git call at all — zen-free stays allowed when the owner has never set a work-repo marker.
func TestIsWorkRepoEmptyMarkersIsPersonal(t *testing.T) {
	origMarkers := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return nil, nil }
	t.Cleanup(func() { workRepoMarkers = origMarkers })
	orig := gitRemoteURLs
	gitRemoteURLs = func(string) ([]string, error) {
		t.Fatal("empty markers must short-circuit before any git call")
		return nil, nil
	}
	t.Cleanup(func() { gitRemoteURLs = orig })
	if IsWorkRepo(t.TempDir()) {
		t.Error("empty markers must report personal, so zen-free stays allowed")
	}
}

// routeRunGit runs git -C dir <args...>, failing the test on error.
func routeRunGit(t *testing.T, dir string, args ...string) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestIsWorkRepoRealGitRemotes exercises IsWorkRepo end to end against real scratch git repos (no seam
// stub), so the actual `git remote -v` parsing in gitRemoteURLs is proven, not just the stub above.
func TestIsWorkRepoRealGitRemotes(t *testing.T) {
	origMarkers := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return []string{"git.corp.example", "corp-widgets"}, nil } // test-only stand-in markers
	t.Cleanup(func() { workRepoMarkers = origMarkers })
	t.Run("personal origin plus corp upstream is work", func(t *testing.T) {
		dir := t.TempDir()
		routeRunGit(t, dir, "init", "-q")
		routeRunGit(t, dir, "remote", "add", "origin", "https://github.com/tester/claude-code.git")
		routeRunGit(t, dir, "remote", "add", "upstream", "https://git.corp.example/digitalbtd/x.git")
		if !IsWorkRepo(dir) {
			t.Error("want work: upstream remote names a corp host")
		}
	})

	t.Run("corp pushurl only is work", func(t *testing.T) {
		dir := t.TempDir()
		routeRunGit(t, dir, "init", "-q")
		routeRunGit(t, dir, "remote", "add", "origin", "https://github.com/tester/claude-code.git")
		routeRunGit(t, dir, "remote", "set-url", "--push", "origin", "https://git.corp.example/digitalbtd/x.git")
		if !IsWorkRepo(dir) {
			t.Error("want work: origin's pushurl names a corp host, even though its fetch url doesn't")
		}
	})

	t.Run("no remotes is personal", func(t *testing.T) {
		dir := t.TempDir()
		routeRunGit(t, dir, "init", "-q")
		if IsWorkRepo(dir) {
			t.Error("want personal: a repo with no remotes at all")
		}
	})

	t.Run("not a git directory is personal", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "not-a-repo")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if IsWorkRepo(dir) {
			t.Error("want personal: a plain directory with no .git at all")
		}
	})
}

// TestIsWorkRepoCorruptConfigFailsClosed pins that a git error inside a real repo reports work, not personal.
func TestIsWorkRepoCorruptConfigFailsClosed(t *testing.T) {
	orig := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return []string{"corp-widgets"}, nil }
	t.Cleanup(func() { workRepoMarkers = orig })
	dir := t.TempDir()
	routeRunGit(t, dir, "init", "-q")
	routeRunGit(t, dir, "remote", "add", "origin", "https://github.com/tester/claude-code.git")
	cfg := filepath.Join(dir, ".git", "config")
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[broken\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !IsWorkRepo(dir) {
		t.Error("want work: rev-parse fails on a bad config, which is not 'not a git repository'")
	}
}

// opencodeCand returns the opencode candidate a pick chose, or fails.
func opencodeCand(t *testing.T, r PickResult) Candidate {
	t.Helper()
	for _, c := range r.Candidates {
		if c.Agent == "opencode" {
			return c
		}
	}
	t.Fatalf("no opencode candidate: %+v skipped %+v", r.Candidates, r.Skipped)
	return Candidate{}
}

func TestOpencodeSubPickNeverFreeAtOpusOrOnWorkRepo(t *testing.T) {
	tb := testTable(t)
	for name, r := range map[string]PickResult{
		"opus":      pickRepo(tb, "opus", State{}, false),
		"work-repo": pickRepo(tb, "sonnet", State{}, true),
	} {
		oc := opencodeCands(r)
		if len(oc) != 1 || oc[0].Billing == BillFree || oc[0].Account != "opencode·go" {
			t.Errorf("%s: opencode candidates = %+v, want exactly one paid opencode·go (no free pick, no backup)", name, oc)
		}
	}
}

// opencodeCands returns every opencode candidate in r, in order.
func opencodeCands(r PickResult) []Candidate {
	var out []Candidate
	for _, c := range r.Candidates {
		if c.Agent == "opencode" {
			out = append(out, c)
		}
	}
	return out
}

// TestOpencodeSubPickAdjacentBackupOnPersonalRepo: the free winner carries its paid backup right after
// it, both at the front of the pick — score/sort must never separate the pair.
func TestOpencodeSubPickAdjacentBackupOnPersonalRepo(t *testing.T) {
	r := pickRepo(testTable(t), "sonnet", State{}, false)
	oc := opencodeCands(r)
	if len(oc) != 2 || oc[0].Account != "opencode·zen-free" || oc[0].Billing != BillFree ||
		oc[1].Account != "opencode·go" || oc[1].Billing == BillFree {
		t.Fatalf("opencode candidates = %+v, want [zen-free, go] adjacent", oc)
	}
	for i, c := range r.Candidates {
		if c.Agent == "opencode" {
			if i+1 >= len(r.Candidates) || r.Candidates[i+1].Agent != "opencode" {
				t.Fatalf("free+backup must sit adjacent, got %s", runners(r))
			}
			break
		}
	}
}

// TestOpencodeSubPickZenFreeExhaustedFallsToGoAlone: an exhausted zen-free account drops out of the sub-pick
// entirely — opencode·go wins alone, with no backup pair (there is nothing left to pair it with).
func TestOpencodeSubPickZenFreeExhaustedFallsToGoAlone(t *testing.T) {
	st := State{"opencode·zen-free": {UsedPct: 100, Seen: now0, ExhaustedUntil: now0.Add(time.Hour)}}
	r := pickRepo(testTable(t), "sonnet", st, false)
	oc := opencodeCands(r)
	if len(oc) != 1 || oc[0].Account != "opencode·go" || oc[0].Billing == BillFree {
		t.Fatalf("exhausted zen-free: opencode candidates = %+v, want opencode·go alone", oc)
	}
	found := false
	for _, s := range r.Skipped {
		if s.Agent == "opencode·zen-free" && s.Gate == GateExhausted {
			found = true
		}
	}
	if !found {
		t.Errorf("skip reasons = %+v, want opencode·zen-free reported exhausted", r.Skipped)
	}
}

// denyOpencode fails every opencode cell (simulating no opencode login at all) and passes every other agent.
func denyOpencode(agent string, _ Cell) error {
	if agent == "opencode" {
		return errors.New("not configured: no opencode login")
	}
	return nil
}

// TestOpencodeSubPickNoLoginSkipsAndChainContinues: with no opencode login at all, every sub-candidate is
// skipped not-configured and the outer chain still produces other runners' candidates.
func TestOpencodeSubPickNoLoginSkipsAndChainContinues(t *testing.T) {
	tb := testTable(t)
	tb.OpencodeLadder = []string{"opencode-go", "github-copilot", "openai"}
	r := tb.Pick(context.Background(), "sonnet", "", State{}, PickOpts{Fallback: FallbackAgents, Configured: denyOpencode,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false }})
	if len(opencodeCands(r)) != 0 {
		t.Fatalf("opencode must be skipped entirely, got %s", runners(r))
	}
	if len(r.Candidates) == 0 {
		t.Fatal("the chain must continue past opencode to the other vendors")
	}
	var reasons int
	for _, s := range r.Skipped {
		if strings.HasPrefix(s.Agent, "opencode") && strings.Contains(s.Reason, "not configured") {
			reasons++
		}
	}
	if reasons == 0 {
		t.Errorf("skipped = %+v, want opencode sub-candidates reported not-configured", r.Skipped)
	}
}

// TestFreeTierOKGateBlocksSonnetAllowsHaiku checks the FreeTierOK gate directly: a registry that only
// enables zen-free at haiku must block the free dispatch at sonnet (falling to the paid opencode·go dispatch) while
// still serving the free dispatch at haiku.
func TestFreeTierOKGateBlocksSonnetAllowsHaiku(t *testing.T) {
	reg := &providers.Registry{Routing: providers.Routing{Opencode: providers.OpencodeRouting{FreeTiers: &[]string{"haiku"}}}}
	tb, err := LoadFrom(reg, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	sonnet := opencodeCand(t, pickRepo(tb, "sonnet", State{}, false))
	if sonnet.Billing == BillFree {
		t.Errorf("sonnet pick = %s (%s), want the free dispatch blocked (freeTiers = [haiku] only)", sonnet.Account, sonnet.Billing)
	}
	haiku := opencodeCand(t, pickRepo(tb, "haiku", State{}, false))
	if haiku.Account != "opencode·zen-free" || haiku.Billing != BillFree {
		t.Errorf("haiku pick = %s (%s), want the free Zen dispatch (freeTiers includes haiku)", haiku.Account, haiku.Billing)
	}
}

// TestOpencodeSubPickOverlayReorderRespected: an overlay that reorders opencodeLadder changes which paid
// backend rides as the free winner's backup — the free dispatch itself still always leads.
func TestOpencodeSubPickOverlayReorderRespected(t *testing.T) {
	tb := testTable(t)
	tb.OpencodeLadder = []string{"github-copilot", "opencode-go", "openai"} // overlay: copilot ahead of Go
	r := tb.Pick(context.Background(), "sonnet", "", State{}, PickOpts{Fallback: FallbackAgents, Configured: allConfigured,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false }})
	oc := opencodeCands(r)
	if len(oc) != 2 || oc[0].Account != "opencode·zen-free" {
		t.Fatalf("opencode candidates = %+v, want the free dispatch still leading", oc)
	}
	if oc[1].Account != "opencode·github-copilot" {
		t.Errorf("backup = %s, want the reordered ladder's new first paid entry (github-copilot)", oc[1].Account)
	}
}

// IsWorkRepo asks git once per dir per process: a remote dropped after the first answer is not re-read.
func TestIsWorkRepoCachesGitPerDir(t *testing.T) {
	orig := workRepoMarkers
	workRepoMarkers = func() ([]string, error) { return []string{"corp-widgets"}, nil }
	t.Cleanup(func() { workRepoMarkers = orig })
	dir := t.TempDir()
	routeRunGit(t, dir, "init", "-q")
	routeRunGit(t, dir, "remote", "add", "origin", "https://github.com/corp-widgets/x.git")
	if !IsWorkRepo(dir) {
		t.Fatal("want work: origin names the corp org")
	}
	routeRunGit(t, dir, "remote", "remove", "origin")
	if !IsWorkRepo(dir) {
		t.Error("second IsWorkRepo re-ran git; want the cached answer")
	}
}
