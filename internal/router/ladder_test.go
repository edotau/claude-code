package router

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestPickFollowsLadder: the seeded ladders order the agents after want, expand copilot's rung models, place claude
// into the first rung after the leading one that it beats; a provider confinement keeps that order on its accounts only.
func TestPickFollowsLadder(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tb, err := LoadFrom(&providers.Registry{Routing: providers.DefaultRouting()}, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	st := State{"anthropic·ultra": {UsedPct: 5, Seen: now0}} // claude outscores copilot (0.75) yet must still follow it
	pickAt := func(route string) PickResult {
		confine(t, route)
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
	if got := order(pickAt("")); !slices.Equal(got, def) {
		t.Errorf("default ladder order =\n%v\nwant\n%v", got, def)
	}
	r := pickAt("github")
	if got := order(r); !slices.Equal(got, def[:2]) {
		t.Errorf("order under github =\n%v\nwant\n%v", got, def[:2])
	}
	if !slices.Contains(r.Skipped, Skip{"opencode·zen-free", gateLadder, "outside github"}) {
		t.Errorf("skips under github = %+v, want opencode·zen-free dropped as outside github", r.Skipped)
	}
}

func TestPickProviderFiltersWantedResolvedAccount(t *testing.T) {
	tb := testTable(t)
	confine(t, "")
	opts := PickOpts{Fallback: FallbackNone, Configured: allConfigured,
		Now:     func() time.Time { return now0 },
		Account: func(string, Cell) (string, string) { return "databricks·dev", BillCredits }}
	baseline := tb.Pick(context.Background(), "sonnet", "codex", State{}, opts)
	if len(baseline.Candidates) != 1 || baseline.Candidates[0].Account != "databricks·dev" {
		t.Fatalf("default route did not admit wanted resolved account: %+v", baseline)
	}
	confine(t, "openai")
	got := tb.Pick(context.Background(), "sonnet", "codex", State{}, opts)
	if len(got.Candidates) != 0 {
		t.Errorf("wanted account outside openai remained eligible: %+v", got.Candidates)
	}
	if !slices.Contains(got.Skipped, Skip{"codex·databricks·dev", gateLadder, "outside openai"}) {
		t.Errorf("skips = %+v, want wanted resolved account rejected as outside openai", got.Skipped)
	}
}

func TestPickRejectsFallbackWithUnservableRungModel(t *testing.T) {
	tb := testTable(t)
	confine(t, "")
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

// TestProviderConfinementWithoutLadderStillFilters: no stored ladder at all must not turn the provider filter off.
func TestProviderConfinementWithoutLadderStillFilters(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tb, err := LoadFrom(&providers.Registry{Routing: providers.DefaultRouting()}, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	tb.Ladders = nil
	confine(t, "anthropic:ultra")
	r := tb.Pick(context.Background(), "sonnet", "", State{}, PickOpts{Fallback: FallbackAll, Configured: allConfigured,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false }})
	if len(r.Candidates) != 1 || r.Candidates[0].Account != "anthropic·ultra" {
		t.Errorf("candidates = %+v, want only anthropic·ultra with no ladder", r.Candidates)
	}
	if !slices.Contains(r.Skipped, Skip{"opencode·zen-free", gateLadder, "outside anthropic:ultra"}) {
		t.Errorf("skips = %+v, want opencode·zen-free dropped as outside anthropic:ultra", r.Skipped)
	}
}

// confine sets this process's confinement to provider[:profile] the way an ask exports it: an inline $COWORKER_OVERLAY.
func confine(t *testing.T, v string) {
	provider, profile, _ := strings.Cut(v, ":")
	doc, _ := json.Marshal(map[string]string{"provider": provider, "profile": profile})
	t.Setenv(overlayEnv, string(doc))
}

func TestParseCoworker(t *testing.T) {
	for _, bad := range []string{"databricks:", "nope", "databricks:a:b", "default", "first-party", "local", "anthropic:", ":dev"} {
		if _, _, err := ParseCoworker(bad); err == nil {
			t.Errorf("ParseCoworker(%q) accepted", bad)
		}
	}
	for in, want := range map[string][2]string{"": {}, " anthropic ": {"anthropic"}, "github:personal": {"github", "personal"},
		"databricks": {RouteDatabricks}, "databricks:dev": {RouteDatabricks, "dev"}} {
		provider, profile, err := ParseCoworker(in)
		if err != nil || [2]string{provider, profile} != want {
			t.Errorf("ParseCoworker(%q) = %q, %q, %v; want %q", in, provider, profile, err, want)
		}
	}
}

// armPick runs want's pick under route databricks:dev on a hermetic registry: dev and qa workspaces, the built-in
// own-login rows and a keyed openai row; codex is pinned to the keyed row and copilot to qa.
func armPick(t *testing.T, want, fallback string) PickResult {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+
		`"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}},`+
		`"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	for _, n := range []string{"DATABRICKS_DEV_PAT", "DATABRICKS_QA_PAT", "OPENAI_API_KEY"} {
		vaulttest.Secret(t, n, "test-value")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	tb, err := LoadFrom(reg, "work-user")
	if err != nil {
		t.Fatal(err)
	}
	confine(t, "databricks:dev")
	return tb.Pick(context.Background(), "sonnet", want, State{}, PickOpts{Fallback: fallback, Configured: allConfigured,
		Now: func() time.Time { return now0 }, WorkRepo: func() bool { return false },
		Pins: map[string]PinSpec{"codex": {Profile: "openai"}, "copilot": {Profile: "qa"}}})
}

// TestArmPickOrder: tier 1 is every agent on databricks·dev, wanted first; tier 2 the first-party logins in ladder
// order; pins elsewhere are skipped as outside the route; none keeps one candidate and agents drops claude.
func TestArmPickOrder(t *testing.T) {
	r := armPick(t, "codex", FallbackAll)
	tier1, tier2 := armTiers(r)
	if len(tier1) != 5 || tier1[0] != "codex" || !slices.Contains(tier1, "claude") {
		t.Errorf("tier 1 = %v, want codex first then the other four agents on databricks·dev", tier1)
	}
	wantTier2 := []string{"copilot/github·personal", "codex/openai·chatgpt", "gemini/google·antigravity",
		"opencode/opencode·go", "claude/anthropic·ultra"}
	if !slices.Equal(tier2, wantTier2) {
		t.Errorf("tier 2 = %v, want %v", tier2, wantTier2)
	}
	for _, label := range []string{"codex·openai·openai", "copilot·databricks·qa"} {
		if !slices.Contains(r.Skipped, Skip{label, gateLadder, "outside databricks:dev"}) {
			t.Errorf("skips = %+v, want %s outside databricks:dev", r.Skipped, label)
		}
	}
	if none := armPick(t, "codex", FallbackNone); len(none.Candidates) != 1 || none.Candidates[0].Account != "databricks·dev" {
		t.Errorf("fallback none = %+v, want codex on databricks·dev only", none.Candidates)
	}
	if ag := armPick(t, "codex", FallbackAgents); slices.ContainsFunc(ag.Candidates, func(c Candidate) bool { return c.Agent == "claude" }) {
		t.Errorf("fallback agents ran claude: %+v", ag.Candidates)
	}
}

// armTiers splits r's candidates at the first one off databricks·dev: tier-1 agents, then tier-2 agent/account.
func armTiers(r PickResult) (tier1, tier2 []string) {
	for _, c := range r.Candidates {
		if c.Account == "databricks·dev" && len(tier2) == 0 {
			tier1 = append(tier1, c.Agent)
		} else {
			tier2 = append(tier2, c.Agent+"/"+c.Account)
		}
	}
	return tier1, tier2
}

// TestArmAdmitDropsOffRouteAccount: the keep rule drops an account that is neither the workspace nor first-party.
func TestArmAdmitDropsOffRouteAccount(t *testing.T) {
	l := pickLadder{arm: "dev", armAccount: "databricks·dev"}
	res := &PickResult{}
	got := l.admit([]Candidate{{Agent: "codex", Account: "databricks·dev"},
		{Agent: "codex", Account: "openai·chatgpt"}, {Agent: "opencode", Account: "opencode·zen-free"},
		{Agent: "codex", Account: "databricks·qa"}}, res)
	if len(got) != 2 || got[0].Account != "databricks·dev" || got[1].Account != "openai·chatgpt" {
		t.Errorf("admitted %+v, want dev and chatgpt", got)
	}
	want := []Skip{{"opencode·zen-free", gateLadder, "outside databricks:dev"}, {"codex·databricks·qa", gateLadder, "outside databricks:dev"}}
	if !slices.Equal(res.Skipped, want) {
		t.Errorf("skips = %+v, want %+v", res.Skipped, want)
	}
}
