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
	got := accountsOf(vendorPick(t, "gemini", true, nil, "dev"), "gemini")
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
