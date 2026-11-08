package router

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func leadPick(t *testing.T, want, lead string) PickResult {
	t.Helper()
	tb := testTable(t)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "")
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	return tb.Pick(context.Background(), "sonnet", want, State{}, PickOpts{
		Configured: allConfigured, Now: func() time.Time { return now0 }, Lead: lead})
}

func TestPickLeadPutsWantedAgentOnLeadProfileFirst(t *testing.T) {
	r := leadPick(t, "codex", "dev")
	if len(r.Candidates) == 0 || r.Candidates[0].Agent != "codex" || r.Candidates[0].Profile != "dev" {
		t.Fatalf("Lead=dev: candidates = %+v, want codex on profile dev first (skipped %+v)", r.Candidates, r.Skipped)
	}
}

func TestPickLeadGeminiNeverLeadsOnNonGoogleRow(t *testing.T) {
	r := leadPick(t, "gemini", "dev")
	for _, c := range r.Candidates {
		if c.Agent == "gemini" && c.Profile == "dev" {
			t.Fatalf("gemini led on non-Google row dev: %+v", c)
		}
	}
	if !slices.ContainsFunc(r.Skipped, func(s Skip) bool {
		return s.Agent == "gemini" && strings.Contains(s.Reason, "gemini runs on Google only, not dev")
	}) {
		t.Fatalf("no gemini Google-only skip: %+v", r.Skipped)
	}
}

func TestGoogleRow(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"chatgpt": {Name: "chatgpt", Provider: "openai", Auth: providers.Auth{Login: "codex"}},
		"g":       {Name: "g", Provider: "google"},
	}}
	if !googleRow(reg, "g") || googleRow(reg, "chatgpt") || googleRow(reg, "missing") {
		t.Error("googleRow must be true only for a provider google row")
	}
}

func TestGateLeadKeepsOtherAgentOnLeadAccount(t *testing.T) {
	tb := testTable(t)
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	reg, _ := providers.Load()
	hop := Candidate{Agent: "copilot", Account: "databricks·dev", Model: "hop-marker"}
	var res PickResult
	got := tb.gateLead(context.Background(), &pickGate{reg: reg, held: map[string]bool{}}, "sonnet", "codex", PinSpec{},
		[]Candidate{hop}, State{}, PickOpts{Lead: "dev"}, now0, allConfigured, &res)
	has := func(agent string) bool {
		return slices.ContainsFunc(got, func(c Candidate) bool { return c.Agent == agent && c.Account == "databricks·dev" })
	}
	if !has("copilot") || !has("codex") {
		t.Fatalf("want both codex lead row and copilot hop on the lead account: %+v (skipped %+v)", got, res.Skipped)
	}
}

func TestPickLeadKeepsBaseRoutesOnceAndLeavesOtherAgents(t *testing.T) {
	r := leadPick(t, "codex", "dev")
	seen := map[string]int{}
	for _, c := range r.Candidates {
		seen[c.Agent+"|"+c.Account]++
		if c.Agent != "codex" && c.Profile != "" {
			t.Errorf("Lead moved non-wanted agent %s onto profile %q", c.Agent, c.Profile)
		}
	}
	if seen["codex|openai·chatgpt"] != 1 || seen["copilot|github·work-user"] != 1 {
		t.Fatalf("base routes lost or repeated: %v", seen)
	}
}

func TestPickWithoutLeadAddsNoLeadProfile(t *testing.T) {
	r := leadPick(t, "codex", "")
	for _, c := range r.Candidates {
		if c.Profile == "dev" {
			t.Fatalf("no Lead set but candidate on dev: %+v", c)
		}
	}
}

func TestGateLeadWithoutRegistryRecordsSkip(t *testing.T) {
	var res PickResult
	base := []Candidate{{Agent: "codex"}}
	got := (&RouteTable{}).gateLead(context.Background(), &pickGate{}, "sonnet", "codex", PinSpec{}, base,
		State{}, PickOpts{Lead: "dev"}, now0, allConfigured, &res)
	if len(got) != 1 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "no provider registry") {
		t.Fatalf("got %+v skipped %+v, want base kept and a registry skip", got, res.Skipped)
	}
}

func TestGateLeadKeepsDistinctBaseAndDropsRepeats(t *testing.T) {
	tb := testTable(t)
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	reg, _ := providers.Load()
	base := []Candidate{{Agent: "codex", Account: "x·other"}, {Agent: "codex", Account: "openai·chatgpt", Model: "gpt-6.1-sol"}}
	var res PickResult
	got := tb.gateLead(context.Background(), &pickGate{reg: reg, held: map[string]bool{}}, "sonnet", "codex", PinSpec{},
		base, State{}, PickOpts{Lead: "dev"}, now0, allConfigured, &res)
	var accts []string
	for _, c := range got {
		accts = append(accts, c.Account)
	}
	want := []string{"databricks·dev", "openai·chatgpt", "x·other"}
	if !slices.Equal(accts, want) {
		t.Fatalf("accounts = %v, want %v (skipped %+v)", accts, want, res.Skipped)
	}
}

func TestLoginProfilesReturnsEveryLoginRowInNameOrder(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"work":     {Name: "work", Auth: providers.Auth{Login: "copilot"}},
		"personal": {Name: "personal", Auth: providers.Auth{Login: "copilot"}},
		"chatgpt":  {Name: "chatgpt", Auth: providers.Auth{Login: "codex"}},
	}}
	if got := loginProfiles(reg, "copilot"); !slices.Equal(got, []string{"personal", "work"}) {
		t.Fatalf("loginProfiles(copilot) = %v, want [personal work]", got)
	}
}

func TestPickPinnedVendorFailsClosedWhenPinExhausted(t *testing.T) {
	st := State{dbxAcct: {UsedPct: 100, ExhaustedUntil: now0.Add(time.Hour), Seen: now0}}
	r := vendorPick(t, "codex", true, st, "")
	if got := accountsOf(r, "codex"); len(got) != 0 {
		t.Fatalf("pin exhausted: codex accounts = %v, want none (no own-login fallback)", got)
	}
}

func TestPickOwnLoginRowsCarryNoProfile(t *testing.T) {
	for _, r := range []PickResult{vendorPick(t, "codex", true, nil, ""), vendorPick(t, "codex", false, nil, "dev")} {
		for _, c := range r.Candidates {
			if c.Agent == "codex" && c.Account == dbxChatgpt && c.Profile != "" {
				t.Errorf("own-login candidate carries Profile %q, want empty", c.Profile)
			}
		}
	}
}

func TestGateLeadBaseCandidateWinsDedup(t *testing.T) {
	tb := testTable(t)
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	reg, _ := providers.Load()
	base := []Candidate{{Agent: "codex", Account: dbxChatgpt, Model: "gpt-6.1-sol", Billing: "base-marker"}}
	var res PickResult
	got := tb.gateLead(context.Background(), &pickGate{reg: reg, held: map[string]bool{}}, "sonnet", "codex", PinSpec{},
		base, State{}, PickOpts{Lead: "dev"}, now0, allConfigured, &res)
	i := slices.IndexFunc(got, func(c Candidate) bool { return c.Account == dbxChatgpt })
	if i < 0 || got[i].Billing != "base-marker" || got[i].Profile != "" {
		t.Fatalf("login row = %+v, want the base candidate kept", got)
	}
}

func TestPickLeadQuietLoginRowSkips(t *testing.T) {
	tb := testTable(t)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "")
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	pick := func(deny string) PickResult {
		return tb.Pick(context.Background(), "sonnet", "codex", State{}, PickOpts{Configured: allConfigured, Lead: "dev",
			Now: func() time.Time { return now0 }, CanServe: func(_ string, p *providers.Provider) bool { return p.Name != deny }})
	}
	for _, s := range pick("chatgpt").Skipped {
		if strings.Contains(s.Reason, "chatgpt") {
			t.Errorf("quiet login row reported a skip: %+v", s)
		}
	}
	for _, s := range pick("dev").Skipped {
		if strings.Contains(s.Reason, "pinned profile") {
			t.Errorf("lead row skip still says pinned: %+v", s)
		}
	}
}

func TestPickPinnedGeminiOnNonGoogleRowIsSkipped(t *testing.T) {
	tb := testTable(t)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "")
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	r := tb.Pick(context.Background(), "sonnet", "gemini", State{}, PickOpts{Configured: allConfigured,
		Now: func() time.Time { return now0 }, Pins: map[string]PinSpec{"gemini": {Profile: "dev"}}})
	for _, c := range r.Candidates {
		if c.Agent == "gemini" && c.Profile == "dev" {
			t.Fatalf("pinned gemini ran on non-Google row: %+v", c)
		}
	}
	if !slices.ContainsFunc(r.Skipped, func(s Skip) bool { return strings.Contains(s.Reason, "gemini runs on Google only, not dev") }) {
		t.Fatalf("no Google-only skip: %+v", r.Skipped)
	}
}

func TestPickPinnedVendorOffersEveryLoginRow(t *testing.T) {
	tb := testTable(t)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "dev")
	rows := `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
		`"auth":{"type":"pat"}}},"github":{"zz-second":{"protocol":"openai","base_url":"https://api.githubcopilot.com",` +
		`"auth":{"type":"cli","login":"copilot"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", rows)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	r := tb.Pick(context.Background(), "sonnet", "codex", State{}, PickOpts{Configured: allConfigured,
		Now: func() time.Time { return now0 }, Pins: map[string]PinSpec{"copilot": {Profile: "dev"}}})
	got := accountsOf(r, "copilot")
	if !slices.Contains(got, "github·personal") || !slices.Contains(got, "github·zz-second") {
		t.Fatalf("copilot accounts = %v, want every login row (skipped %+v)", got, r.Skipped)
	}
}
