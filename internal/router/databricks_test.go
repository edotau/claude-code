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

const routeDbxRow = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
	`"auth":{"type":"pat"}}}}}`

// dbxCandidates seeds a hermetic vault (a signed-in Databricks row "dev", the given menu), runs a codex pick
// under session profile and returns the codex candidates' accounts plus the pick.
func dbxCandidates(t *testing.T, profile string, menu []providers.OpencodeModel, st State) ([]string, PickResult) {
	t.Helper()
	tb := testTable(t)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", profile)
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	r := tb.Pick(context.Background(), "sonnet", "codex", st, PickOpts{Configured: allConfigured, Now: func() time.Time { return now0 }})
	var accts []string
	for _, c := range r.Candidates {
		if c.Agent == "codex" {
			accts = append(accts, c.Account)
		}
	}
	return accts, r
}

func hasDbxSkip(r PickResult, gate string) bool {
	return slices.ContainsFunc(r.Skipped, func(s Skip) bool {
		return s.Agent == "codex·databricks·dev" && (gate == "" || s.Gate == gate)
	})
}

var (
	dbxGPT          = providers.OpencodeModel{ID: "gpt-6-1-sol", Label: "GPT", Responses: true}
	dbxClaudeNoResp = providers.OpencodeModel{ID: "claude-opus-5-5", Label: "Opus"}
	dbxClaudeResp   = providers.OpencodeModel{ID: "claude-sonnet-5-5", Label: "Sonnet", Responses: true}
)

const (
	dbxChatgpt = "openai·chatgpt"
	dbxAcct    = "databricks·dev"
)

// Databricks on + Responses model: candidate right behind ChatGPT; the non-Responses Claude entry is skipped over.
func TestCodexDatabricksCandidateResponsesModel(t *testing.T) {
	accts, r := dbxCandidates(t, "dev", []providers.OpencodeModel{dbxClaudeNoResp, dbxGPT}, nil)
	if !slices.Equal(accts, []string{dbxChatgpt, dbxAcct}) {
		t.Fatalf("accounts = %v, want [%s %s]", accts, dbxChatgpt, dbxAcct)
	}
	d := r.Candidates[slices.IndexFunc(r.Candidates, func(c Candidate) bool { return c.Account == dbxAcct })]
	if d.Profile != "dev" || d.Model == "" || d.Model == dbxClaudeNoResp.ID || !slices.Contains([]string{dbxGPT.ID, "databricks-" + dbxGPT.ID}, d.Model) {
		t.Errorf("databricks candidate = %+v, want profile dev and the GPT wire id, not the non-Responses Claude", d)
	}
}

// A Responses-flagged Claude id counts as usable.
func TestCodexDatabricksCandidateResponsesFlaggedClaude(t *testing.T) {
	accts, _ := dbxCandidates(t, "dev", []providers.OpencodeModel{dbxClaudeResp}, nil)
	if !slices.Equal(accts, []string{dbxChatgpt, dbxAcct}) {
		t.Errorf("Responses-flagged Claude: accounts = %v", accts)
	}
}

// No Responses model served: routing unchanged, no skip noise.
func TestCodexDatabricksCandidateNoResponsesModel(t *testing.T) {
	accts, r := dbxCandidates(t, "dev", []providers.OpencodeModel{dbxClaudeNoResp}, nil)
	if !slices.Equal(accts, []string{dbxChatgpt}) || hasDbxSkip(r, "") {
		t.Errorf("no Responses model: accounts = %v skipped = %+v", accts, r.Skipped)
	}
}

// Databricks off (session on a non-Databricks profile): ChatGPT only, no skip.
func TestCodexDatabricksCandidateOff(t *testing.T) {
	accts, r := dbxCandidates(t, "personal", []providers.OpencodeModel{dbxGPT}, nil)
	if !slices.Equal(accts, []string{dbxChatgpt}) || hasDbxSkip(r, "") {
		t.Errorf("databricks off: accounts = %v skipped = %+v", accts, r.Skipped)
	}
}

// Exhausted Databricks account: no candidate, one matching skip.
func TestCodexDatabricksCandidateExhausted(t *testing.T) {
	st := State{dbxAcct: {UsedPct: 100, ExhaustedUntil: now0.Add(3600e9), Seen: now0}}
	accts, r := dbxCandidates(t, "dev", []providers.OpencodeModel{dbxGPT}, st)
	if !slices.Equal(accts, []string{dbxChatgpt}) || !hasDbxSkip(r, GateExhausted) {
		t.Errorf("exhausted: accounts = %v skipped = %+v", accts, r.Skipped)
	}
}

// vendorPick runs a pick for want with codex and copilot pinned to Databricks row dev.
func vendorPick(t *testing.T, want string, pinned bool, st State, lead string) PickResult {
	t.Helper()
	tb := testTable(t)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "dev")
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	o := PickOpts{Configured: allConfigured, Now: func() time.Time { return now0 }, Lead: lead}
	if pinned {
		o.Pins = map[string]PinSpec{"codex": {Profile: "dev"}, "copilot": {Profile: "dev"}}
	}
	return tb.Pick(context.Background(), "sonnet", want, st, o)
}

func accountsOf(r PickResult, agent string) []string {
	var accts []string
	for _, c := range r.Candidates {
		if c.Agent == agent {
			accts = append(accts, c.Account)
		}
	}
	return accts
}

func TestPickVendorAfterDatabricksPin(t *testing.T) {
	r := vendorPick(t, "copilot", true, nil, "")
	if got := accountsOf(r, "codex"); !slices.Equal(got, []string{dbxAcct, dbxChatgpt}) {
		t.Errorf("non-want codex accounts = %v, want [%s %s] (skipped %+v)", got, dbxAcct, dbxChatgpt, r.Skipped)
	}
	if got := accountsOf(r, "copilot"); slices.Contains(got, "github·personal") {
		t.Errorf("wanted copilot accounts = %v, want the pin only without Lead", got)
	}
}

func TestPickUnpinnedSessionDefaultOrder(t *testing.T) {
	if got := accountsOf(vendorPick(t, "codex", false, nil, ""), "codex"); !slices.Equal(got, []string{dbxChatgpt, dbxAcct}) {
		t.Errorf("unpinned codex = %v, want [%s %s]", got, dbxChatgpt, dbxAcct)
	}
	if got := accountsOf(vendorPick(t, "codex", false, nil, "dev"), "codex"); !slices.Equal(got, []string{dbxAcct, dbxChatgpt}) {
		t.Errorf("unpinned codex, lead dev = %v, want [%s %s]", got, dbxAcct, dbxChatgpt)
	}
}

func TestPickGeminiStaysOnGoogle(t *testing.T) {
	got := accountsOf(vendorPick(t, "gemini", true, nil, ""), "gemini") // no lead: the default path stays on Google
	if len(got) == 0 {
		t.Fatal("no gemini candidate")
	}
	for _, a := range got {
		if !strings.HasPrefix(a, "google·") {
			t.Errorf("gemini on %s, want google only", a)
		}
	}
}

func TestPickVendorSkipsExhausted(t *testing.T) {
	st := State{dbxChatgpt: Entry{UsedPct: 100, Seen: now0, ResetsAt: now0.Add(time.Hour)}}
	r := vendorPick(t, "copilot", true, st, "")
	if got := accountsOf(r, "codex"); !slices.Equal(got, []string{dbxAcct}) {
		t.Errorf("codex accounts = %v, want [%s]", got, dbxAcct)
	}
	if !slices.ContainsFunc(r.Skipped, func(s Skip) bool { return s.Gate == GateExhausted }) {
		t.Errorf("no exhausted skip: %+v", r.Skipped)
	}
}

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

// A lead naming a Databricks gateway row runs gemini there (the all-Databricks arm, on command).
func TestPickLeadGeminiLeadsOnNamedDatabricksRow(t *testing.T) {
	r := leadPick(t, "gemini", "dev")
	if len(r.Candidates) == 0 || r.Candidates[0].Agent != "gemini" || r.Candidates[0].Profile != "dev" {
		t.Fatalf("gemini did not lead on dev: %+v (skipped %+v)", r.Candidates, r.Skipped)
	}
}

func TestGeminiRow(t *testing.T) {
	dbx := func(a providers.Auth) *providers.Provider {
		return &providers.Provider{Preset: providers.PresetDatabricks, Auth: a}
	}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{
		"chatgpt":  {Name: "chatgpt", Provider: "openai", Auth: providers.Auth{Login: "codex", Type: "cli"}},
		"g":        {Name: "g", Provider: "google"},
		"dev":      dbx(providers.Auth{Type: providers.AuthPAT}),
		"dbxlogin": dbx(providers.Auth{Type: providers.AuthPAT, Login: "databricks"}),
		"dbxnone":  dbx(providers.Auth{Type: providers.AuthNone}),
		"dbxcli":   dbx(providers.Auth{Type: providers.AuthCLI}),
	}}
	for name, want := range map[string]bool{"g": true, "dev": true, "chatgpt": false, "missing": false,
		"dbxlogin": false, "dbxnone": false, "dbxcli": false} {
		if got := geminiRow(reg, name); got != want {
			t.Errorf("geminiRow(%q) = %v, want %v: Google rows and credentialed Databricks gateway rows only", name, got, want)
		}
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

// A gemini pin on a Databricks gateway row is the owner's choice and runs there.
func TestPickPinnedGeminiOnDatabricksRowRuns(t *testing.T) {
	tb := testTable(t)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "")
	vaulttest.Put(t, vault.Overlay, "providers", routeDbxRow)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	r := tb.Pick(context.Background(), "sonnet", "gemini", State{}, PickOpts{Configured: allConfigured,
		Now: func() time.Time { return now0 }, Pins: map[string]PinSpec{"gemini": {Profile: "dev"}}})
	if !slices.ContainsFunc(r.Candidates, func(c Candidate) bool { return c.Agent == "gemini" && c.Profile == "dev" }) {
		t.Fatalf("pinned gemini not on dev: %+v (skipped %+v)", r.Candidates, r.Skipped)
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

const routeDbx3Rows = `{"providers":{"databricks":{` +
	`"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}},` +
	`"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}},` +
	`"prod":{"preset":"databricks","host":"https://prod.cloud.databricks.com","auth":{"type":"pat"}}}}}`

func workspacePick(t *testing.T, want, lead, pin, fb string) []string {
	t.Helper()
	tb := testTable(t)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "")
	vaulttest.Put(t, vault.Overlay, "providers", routeDbx3Rows)
	for _, n := range []string{"DEV", "QA", "PROD"} {
		vaulttest.Secret(t, "DATABRICKS_"+n+"_PAT", "dapi-test")
	}
	r := tb.Pick(context.Background(), "sonnet", want, State{}, PickOpts{
		Configured: allConfigured, Now: func() time.Time { return now0 }, Lead: lead, Fallback: fb,
		Pins: map[string]PinSpec{want: {Profile: pin}}})
	var accts []string
	for _, c := range r.Candidates {
		if c.Agent == want {
			accts = append(accts, c.Account)
		}
	}
	return accts
}

func TestPickLeadOrdersWorkspacesThenOwnLogin(t *testing.T) {
	got := workspacePick(t, "codex", "prod", "dev", FallbackAll)
	want := []string{"databricks·prod", "databricks·dev", "databricks·qa", "openai·chatgpt"}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("codex accounts = %v, want prefix %v", got, want)
	}
}

func TestPickPinnedWithoutLeadAddsSiblingWorkspaces(t *testing.T) {
	got := workspacePick(t, "codex", "", "dev", FallbackAll)
	want := []string{"databricks·dev", "databricks·qa", "databricks·prod"}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("codex accounts = %v, want prefix %v", got, want)
	}
}

// Under --fallback none a lead adds no sibling workspace; its own-login row stays, as before the sweep.
func TestPickLeadFallbackNoneSkipsSiblingWorkspaces(t *testing.T) {
	got := workspacePick(t, "codex", "prod", "prod", FallbackNone)
	if want := []string{"databricks·prod", "openai·chatgpt"}; !slices.Equal(got, want) {
		t.Fatalf("codex accounts = %v, want %v", got, want)
	}
}

// A discovery pin stamps the dispatched workspace only; sibling rows resolve a pool model but write nothing.
func TestPickDiscoveryPinDoesNotStampSiblingWorkspaces(t *testing.T) {
	tb := testTable(t)
	vaulttest.Use(t)
	t.Setenv("CLAUDE_PROFILE", "")
	vaulttest.Put(t, vault.Overlay, "providers", routeDbx3Rows)
	for _, n := range []string{"DEV", "QA", "PROD"} {
		vaulttest.Secret(t, "DATABRICKS_"+n+"_PAT", "dapi-test")
	}
	seed := now0.Add(-time.Hour)
	if err := update(func(s State) { s["databricks·qa"] = Entry{Discovery: map[string]time.Time{"m-seed": seed}} }); err != nil {
		t.Fatal(err)
	}
	sibling := func() string {
		b, err := json.Marshal([]Entry{LoadState()["databricks·qa"], LoadState()["databricks·prod"]})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	before := sibling()
	r := tb.Pick(context.Background(), "sonnet", "copilot", State{}, PickOpts{Configured: allConfigured,
		Now: func() time.Time { return now0 }, Pins: map[string]PinSpec{"copilot": {Profile: "dev", Model: DiscoveryModel}}})
	if after := sibling(); after != before {
		t.Fatalf("sibling state changed:\nbefore %s\nafter  %s", before, after)
	}
	if len(LoadState()["databricks·dev"].Discovery) == 0 {
		t.Fatal("dispatched workspace was not stamped (positive control)")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Candidates {
		if c.Agent == "copilot" && c.Account == "databricks·qa" && !IsPoolModel(reg.Providers["qa"], c.Model) {
			t.Errorf("sibling qa model = %q, want a pool model", c.Model)
		}
	}
}
