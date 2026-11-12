package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestCheckModel1MAliasPassesLikeBareAlias: codex on a Databricks profile treats <alias>[1m] exactly as the bare alias.
func TestCheckModel1MAliasPassesLikeBareAlias(t *testing.T) {
	vaulttest.Use(t)
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Host: "https://dev.example.com"}
	now := time.Now()
	for _, alias := range []string{"opus", "sonnet", "haiku"} {
		bareWire, bareFailed, bareNote := checkModel("codex", p, alias, now)
		wire, failed, note := checkModel("codex", p, alias+"[1m]", now)
		if failed != nil || bareFailed != nil || note != bareNote || wire != alias+"[1m]" || bareWire != alias {
			t.Errorf("%s[1m]: wire %q failed %v note %q; bare %s: wire %q failed %v note %q",
				alias, wire, failed, note, alias, bareWire, bareFailed, bareNote)
		}
	}
}

// A preview leads only with an explicit lead; the session profile alone adds none (callers behave as before).
func TestPreviewRouteLeadsOnlyWhenAsked(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
	t.Setenv("CLAUDE_PROFILE", "no-such-row")
	for _, lead := range []string{"", "no-such-row"} {
		q := RouteReq{Agent: "codex", Task: "x", Lead: lead, Fallback: router.FallbackAll, Preview: true}
		_, res, err := Resolve(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		var skips []string
		for _, s := range res.Skipped {
			skips = append(skips, s.Reason)
		}
		if got := strings.Contains(strings.Join(skips, "\n"), "no-such-row"); got != (lead != "") {
			t.Fatalf("lead %q: skip naming no-such-row = %v, skipped %+v", lead, got, res.Skipped)
		}
	}
}

// A candidate dropped after the pick must not lend its slot to the next survivor of the same agent and account.
func TestPreviewOutcomeMatchesByRungNotPosition(t *testing.T) {
	cell := func(m string) router.Cell { return router.Cell{Model: m} }
	pick := router.PickResult{Candidates: []router.Candidate{
		{Agent: "codex", Cell: cell("m1"), Profile: "A"},
		{Agent: "codex", Cell: cell("m2"), Profile: "B"},
	}}
	chain := Chain{Dispatches: []Dispatch{{Agent: &pinFallbackRunner{name: "codex"}, Model: "m2", rung: 2}}}
	got := previewOutcome(chain, pick).Candidates
	if len(got) != 1 || got[0].Profile != "B" || got[0].Model != "m2" {
		t.Fatalf("candidates = %+v, want only rung 2 (profile B, model m2)", got)
	}
}
