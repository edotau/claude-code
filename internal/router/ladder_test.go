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
	st := State{"anthropic·max": {UsedPct: 5, Seen: now0}} // claude outscores copilot (0.75) yet must still follow it
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
		"claude/anthropic·max/claude-sonnet-5-5", "codex/openai·chatgpt/gpt-6.1-sol",
		"opencode/opencode·zen-free/opencode/deepseek-v4.1-flash-free", "opencode/opencode·go/opencode-go/kimi-k3.1-pro",
		"gemini/google·antigravity/gemini-3.8-flash"}
	if got := order(pickAt(RouteDefault)); !slices.Equal(got, def) {
		t.Errorf("default ladder order =\n%v\nwant\n%v", got, def)
	}
	fp := []string{"copilot/github·work-user/claude-sonnet-5.5", "claude/anthropic·max/claude-sonnet-5-5",
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
