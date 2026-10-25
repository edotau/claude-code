package route

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/routetable"
)

func testTable(t *testing.T) *Table {
	t.Helper()
	tb, err := load(nil, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func allConfigured(string, Cell) error { return nil }

var now0 = time.Date(2026, 10, 25, 2, 6, 0, 0, time.Local)

func TestEveryCellResolvesAndTierOfRoundTrips(t *testing.T) {
	tb := testTable(t)
	for _, r := range Runners {
		for _, tier := range Tiers {
			c, ok := tb.Cell(r, tier)
			if !ok || c.Model == "" || c.Account == "" || tb.Billing[c.Account] == "" {
				t.Fatalf("%s/%s: cell %+v ok=%v billing=%q", r, tier, c, ok, tb.Billing[c.Account])
			}
			// codex and opencode serve one id at both opus and sonnet: the strongest tier claims a shared id
			if got := tb.TierOf(c.Model); got != tier && ((r != "codex" && r != "opencode") || tier != "sonnet" || got != "opus") {
				t.Errorf("TierOf(%s) = %q, want %q", c.Model, got, tier)
			}
		}
	}
	if a := tb.Rows["copilot"]["sonnet"].Account; a != "copilot:work-user" {
		t.Errorf("copilot account = %q, want the pinned user only", a)
	}
	if got := tb.TierOf("claude-opus-5-5[1m]"); got != "opus" {
		t.Errorf("TierOf(claude id) = %q", got)
	}
}

func TestOverlayCellAndWeights(t *testing.T) {
	// opencode-go/y keeps the opencode cell's own backend prefix, so the override only replaces the model.
	reg := &providers.Registry{Route: map[string]map[string]providers.RouteCell{"opencode": {"sonnet": {Model: "opencode-go/y"}}},
		RouteWeights: map[string]float64{"price": 0.2, "headroom": 0.8}}
	tb, err := load(reg, "u")
	if err != nil {
		t.Fatal(err)
	}
	if c := tb.Rows["opencode"]["sonnet"]; c.Model != "opencode-go/y" || c.Effort != "high" {
		t.Errorf("overlay cell = %+v, want model replaced and effort kept", c)
	}
	if tb.Weights["headroom"] != 0.8 {
		t.Errorf("weights = %v", tb.Weights)
	}
	bad := &providers.Registry{Route: map[string]map[string]providers.RouteCell{"ghost": {"opus": {Model: "m"}}}}
	if _, err := load(bad, "u"); err == nil {
		t.Error("unknown overlay cell must fail")
	}
}

func TestWeightValidation(t *testing.T) {
	for name, w := range map[string]map[string]float64{
		"sum":     {"price": 0.5, "headroom": 0.6},
		"missing": {"price": 1},
		"extra":   {"price": 0.5, "headroom": 0.5, "latency": 0},
		"range":   {"price": 1.5, "headroom": -0.5},
	} {
		if _, err := load(&providers.Registry{RouteWeights: w}, "u"); err == nil {
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
	if got, _ := ParseReset("resets 2026-10-25T09:00:00Z", now0); !got.Equal(time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("a Z timestamp must keep UTC, got %v", got)
	}
	ny, _ := time.LoadLocation("America/New_York")
	if got := firstOfNextMonth(time.Date(2026, 10, 31, 21, 0, 0, 0, ny)); !got.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("Oct 31 21:00 NY is Nov 1 UTC: next month boundary = %v", got)
	}
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
		{Entry{UsedPct: 50, Seen: seen}, StateOK},
		{Entry{UsedPct: 90, Seen: seen}, StateLow},
		{Entry{UsedPct: 100, Seen: seen, ResetsAt: now0.Add(time.Hour)}, StateExhausted},
		{Entry{UsedPct: 100, Seen: seen, ResetsAt: now0.Add(-time.Hour)}, StateOK},
		{Entry{UsedPct: 5, Seen: seen, ExhaustedUntil: now0.Add(time.Hour)}, StateExhausted},
		{Entry{UsedPct: 5, Seen: seen, ExhaustedUntil: now0.Add(-time.Hour)}, StateOK},
	} {
		if got := c.e.Status(now0); got != c.want {
			t.Errorf("Status(%+v) = %s, want %s", c.e, got, c.want)
		}
	}
}

func pick(tb *Table, tier, want string, st State, mode string) Result {
	return tb.Pick(context.Background(), tier, want, st, Opts{Fallback: mode, Configured: allConfigured, Now: func() time.Time { return now0 }})
}

func runners(r Result) string {
	var s []string
	for _, c := range r.Candidates {
		s = append(s, c.Runner)
	}
	return strings.Join(s, ",")
}

func TestPickGatesBeatScore(t *testing.T) {
	tb := testTable(t)
	st := State{"copilot:work-user": {UsedPct: 0, Seen: now0, ExhaustedUntil: time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local), Reason: "402"}}
	r := pick(tb, "sonnet", "copilot", st, FallbackAll)
	if strings.Contains(runners(r), "copilot") {
		t.Fatalf("exhausted copilot picked: %s", runners(r))
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Gate != GateExhausted || !strings.Contains(r.Skipped[0].Reason, "copilot:work-user exhausted until 2026-11-01") {
		t.Errorf("skips = %+v", r.Skipped)
	}
	if last := r.Candidates[len(r.Candidates)-1]; last.Runner != "claude" || last.Model != "sonnet" {
		t.Errorf("claude leg = %+v, want sonnet tier kept", last)
	}
	unpinned, _ := load(nil, "")
	r = pick(unpinned, "opus", "copilot", State{}, FallbackVendors)
	if r.Skipped[0].Gate != GateAccount || strings.Contains(runners(r), "claude") {
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
		"codex:chatgpt":      {UsedPct: 10, Seen: now0},
		"gemini:antigravity": {UsedPct: 60, Seen: now0},
		"opencode:go":        {UsedPct: 20, Seen: now0},
	}
	r := pick(tb, "sonnet", "", st, FallbackVendors)
	if got := runners(r); got != "codex,opencode,copilot,gemini" {
		t.Errorf("order = %s (scores %+v)", got, r.Candidates)
	}
	c := r.Candidates[0]
	if c.Scores["price"] != 0.9 || c.Scores["headroom"] != 0.9 || c.Total != 0.9 {
		t.Errorf("codex scores = %+v total %v", c.Scores, c.Total)
	}
	st["codex:chatgpt"] = Entry{UsedPct: 95, Seen: now0}
	if got := runners(pick(tb, "sonnet", "", st, FallbackVendors)); !strings.HasSuffix(got, ",codex") {
		t.Errorf("low codex must sort after every non-low runner: %s", got)
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
	cheap := Candidate{Runner: "codex", Billing: BillCredits, Account: "codex:chatgpt", Price: routetable.Price{In: 1, Out: 1}}
	pricey := Candidate{Runner: "gemini", Billing: BillCredits, Account: "gemini:antigravity", Price: routetable.Price{In: 4, Out: 4}}
	st := State{} // both accounts unseen: headroom ties at 0.5 for each
	scored := tb.score([]Candidate{cheap, pricey}, st)
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

func TestStateRoundTripScopedToConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := Record("codex:chatgpt", 40, "5h", now0.Add(time.Hour), now0); err != nil {
		t.Fatal(err)
	}
	if err := MarkExhausted("copilot:u", now0.Add(time.Hour), now0, "402"); err != nil {
		t.Fatal(err)
	}
	if err := MarkExhausted("copilot:u", now0.Add(time.Minute), now0, "short"); err != nil {
		t.Fatal(err)
	}
	s := LoadState()
	if !s["copilot:u"].ExhaustedUntil.Equal(now0.Add(time.Hour)) {
		t.Errorf("a shorter concurrent deadline replaced a longer one: %v", s["copilot:u"].ExhaustedUntil)
	}
	if s["codex:chatgpt"].UsedPct != 40 || s["copilot:u"].Status(now0) != StateExhausted {
		t.Errorf("state = %+v", s)
	}
	if !strings.HasPrefix(StatePath(), dir) {
		t.Errorf("StatePath = %s", StatePath())
	}
}
