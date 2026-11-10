package router

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// TestPickFollowsLadder: the seeded ladders order the agents after want, expand copilot's rung models, place claude
// into the first rung after the leading one that it beats, and firstParty drops a non-first-party account.
func TestPickFollowsLadder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tb, err := LoadFrom(&providers.Registry{Routing: providers.DefaultRouting()}, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	st := State{"anthropic·ultra": {UsedPct: 5, Seen: now0}} // claude outscores copilot (0.75) yet must still follow it
	pickAt := func(route string) PickResult {
		t.Setenv(EnvRoute, route)
		return tb.Pick(context.Background(), "sonnet", "", st, PickOpts{Fallback: FallbackAll, Configured: allConfigured,
			Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false }})
	}
	order := func(r PickResult) []string {
		var out []string
		for _, c := range r.Candidates {
			out = append(out, c.Agent+"/"+c.Account+"/"+c.Model)
		}
		return out
	}
	def := []string{"copilot/github·work-user/gpt-6.1-sol", "copilot/github·work-user/gpt-6-astra",
		"claude/anthropic·ultra/claude-sonnet-5-5", "codex/openai·chatgpt/gpt-6.1-sol",
		"opencode/opencode·zen-free/opencode/deepseek-v4.1-flash-free", "opencode/opencode·go/opencode-go/kimi-k3.1-pro",
		"gemini/google·antigravity/gemini-3.8-flash"}
	if got := order(pickAt(RouteDefault)); !slices.Equal(got, def) {
		t.Errorf("default ladder order =\n%v\nwant\n%v", got, def)
	}
	fp := []string{"copilot/github·work-user/claude-sonnet-5.5", "claude/anthropic·ultra/claude-sonnet-5-5",
		"codex/openai·chatgpt/gpt-6.1-sol", "gemini/google·antigravity/gemini-3.8-flash",
		"opencode/opencode·go/opencode-go/kimi-k3.1-pro"}
	r := pickAt(routeFirstParty)
	if got := order(r); !slices.Equal(got, fp) {
		t.Errorf("firstParty ladder order =\n%v\nwant\n%v", got, fp)
	}
	if !slices.Contains(r.Skipped, Skip{"opencode·zen-free", gateLadder, "not first-party"}) {
		t.Errorf("firstParty skips = %+v, want opencode·zen-free dropped as not first-party", r.Skipped)
	}
}

func TestPickFirstPartyFiltersWantedResolvedAccount(t *testing.T) {
	tb := testTable(t)
	t.Setenv(EnvRoute, RouteDefault)
	opts := PickOpts{Fallback: FallbackNone, Configured: allConfigured,
		Now:     func() time.Time { return now0 },
		Account: func(string, Cell) (string, string) { return "databricks·dev", BillCredits }}
	baseline := tb.Pick(context.Background(), "sonnet", "codex", State{}, opts)
	if len(baseline.Candidates) != 1 || baseline.Candidates[0].Account != "databricks·dev" {
		t.Fatalf("default route did not admit wanted resolved account: %+v", baseline)
	}
	t.Setenv(EnvRoute, routeFirstParty)
	got := tb.Pick(context.Background(), "sonnet", "codex", State{}, opts)
	if len(got.Candidates) != 0 {
		t.Errorf("wanted non-first-party account remained eligible: %+v", got.Candidates)
	}
	if !slices.Contains(got.Skipped, Skip{"codex·databricks·dev", gateLadder, "not first-party"}) {
		t.Errorf("skips = %+v, want wanted resolved account rejected as not first-party", got.Skipped)
	}
}

func TestPickRejectsFallbackWithUnservableRungModel(t *testing.T) {
	tb := testTable(t)
	t.Setenv(EnvRoute, RouteDefault)
	opts := PickOpts{Fallback: FallbackAgents, Configured: allConfigured,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return true }}
	baseline := tb.Pick(context.Background(), "sonnet", "copilot", State{}, opts)
	isCodex := func(c Candidate) bool { return c.Agent == "codex" }
	if !slices.ContainsFunc(baseline.Candidates, isCodex) {
		t.Fatalf("baseline did not admit codex fallback: %+v", baseline)
	}
	tb.Ladders = map[string][]providers.Rung{providers.LadderDefault: {
		{Agent: "codex", Provider: "openai", Models: []string{"databricks-unavailable"}},
		{Agent: "gemini"},
	}}
	got := tb.Pick(context.Background(), "sonnet", "copilot", State{}, opts)
	if slices.ContainsFunc(got.Candidates, isCodex) {
		t.Errorf("fallback survived rejected rung model with its original model: %+v", got.Candidates)
	}
	if !slices.Contains(got.Skipped, Skip{"codex·openai·chatgpt", gateLadder,
		"rung model databricks-unavailable is not served on openai·chatgpt"}) {
		t.Errorf("skips = %+v, want codex fallback rejected for an unservable rung model", got.Skipped)
	}
	if len(got.Candidates) == 0 || got.Candidates[0].Agent != "copilot" {
		t.Errorf("wanted candidate lost while rejecting fallback: %+v", got.Candidates)
	}
}

// TestFirstPartyRouteWithoutLadderStillFilters: an empty first-party ladder must not turn the first-party filter off.
func TestFirstPartyRouteWithoutLadderStillFilters(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tb, err := LoadFrom(&providers.Registry{Routing: providers.DefaultRouting()}, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	tb.Ladders = map[string][]providers.Rung{providers.LadderDefault: tb.Ladders[providers.LadderDefault]}
	t.Setenv(EnvRoute, routeFirstParty)
	r := tb.Pick(context.Background(), "sonnet", "", State{}, PickOpts{Fallback: FallbackAll, Configured: allConfigured,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false }})
	for _, c := range r.Candidates {
		if !firstPartyAccount(c.Account) {
			t.Errorf("candidate %s/%s admitted on first-party route with no first-party ladder", c.Agent, c.Account)
		}
	}
	if !slices.Contains(r.Skipped, Skip{"opencode·zen-free", gateLadder, "not first-party"}) {
		t.Errorf("skips = %+v, want opencode·zen-free dropped as not first-party", r.Skipped)
	}
}
