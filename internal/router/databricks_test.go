package router

import (
	"context"
	"slices"
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

func TestCodexDatabricksCandidate(t *testing.T) {
	gpt := providers.OpencodeModel{ID: "gpt-6-1-sol", Label: "GPT", Responses: true}
	claudeNoResp := providers.OpencodeModel{ID: "claude-opus-5-5", Label: "Opus"}
	claudeResp := providers.OpencodeModel{ID: "claude-sonnet-5-5", Label: "Sonnet", Responses: true}
	chatgpt, dbx := "openai·chatgpt", "databricks·dev"

	// Databricks on + Responses model: candidate right behind ChatGPT; the non-Responses Claude entry is skipped over.
	accts, r := dbxCandidates(t, "dev", []providers.OpencodeModel{claudeNoResp, gpt}, nil)
	if !slices.Equal(accts, []string{chatgpt, dbx}) {
		t.Fatalf("accounts = %v, want [%s %s]", accts, chatgpt, dbx)
	}
	d := r.Candidates[slices.IndexFunc(r.Candidates, func(c Candidate) bool { return c.Account == dbx })]
	if d.Profile != "dev" || d.Model == "" || d.Model == claudeNoResp.ID || !slices.Contains([]string{gpt.ID, "databricks-" + gpt.ID}, d.Model) {
		t.Errorf("databricks candidate = %+v, want profile dev and the GPT wire id, not the non-Responses Claude", d)
	}

	// A Responses-flagged Claude id counts as usable.
	accts, _ = dbxCandidates(t, "dev", []providers.OpencodeModel{claudeResp}, nil)
	if !slices.Equal(accts, []string{chatgpt, dbx}) {
		t.Errorf("Responses-flagged Claude: accounts = %v", accts)
	}

	// No Responses model served: routing unchanged, no skip noise.
	accts, r = dbxCandidates(t, "dev", []providers.OpencodeModel{claudeNoResp}, nil)
	if !slices.Equal(accts, []string{chatgpt}) || hasDbxSkip(r, "") {
		t.Errorf("no Responses model: accounts = %v skipped = %+v", accts, r.Skipped)
	}

	// Databricks off (session on a non-Databricks profile): ChatGPT only, no skip.
	accts, r = dbxCandidates(t, "personal", []providers.OpencodeModel{gpt}, nil)
	if !slices.Equal(accts, []string{chatgpt}) || hasDbxSkip(r, "") {
		t.Errorf("databricks off: accounts = %v skipped = %+v", accts, r.Skipped)
	}

	// Exhausted Databricks account: no candidate, one matching skip.
	st := State{dbx: {UsedPct: 100, ExhaustedUntil: now0.Add(3600e9), Seen: now0}}
	accts, r = dbxCandidates(t, "dev", []providers.OpencodeModel{gpt}, st)
	if !slices.Equal(accts, []string{chatgpt}) || !hasDbxSkip(r, GateExhausted) {
		t.Errorf("exhausted: accounts = %v skipped = %+v", accts, r.Skipped)
	}
}
