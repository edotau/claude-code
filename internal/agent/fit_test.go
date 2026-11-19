package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
)

// Only successes and quality failures reach the fit table; an availability failure counts for nothing.
func TestFitOutcomes(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	rec := func(status, kind string) Record {
		return Record{Time: t0, Agent: "claude", Status: status, FailKind: kind, Model: "claude-sonnet-5-5", DurationMs: 4000,
			Request: RunOptions{Prompt: "Tier: standard\nAdd a flag."}}
	}
	got := FitOutcomes([]Record{rec("ok", ""), rec("error", FailQuality), rec("error", FailAvailability), rec("error", ""), rec("running", "")})
	if len(got) != 2 || !got[0].OK || got[0].Secs != 4 || got[0].Alias != "sonnet" || got[1].OK || got[1].Secs != 0 {
		t.Errorf("FitOutcomes = %+v, want a 4s success and a quality failure", got)
	}
}

// In shadow a failing cell changes no pick; the label only gains the score.
func TestAutoLabelFitShadow(t *testing.T) {
	reg, _, _ := autoFixture(t)
	rankedOnly(t)
	q := ModelQuery{Agent: "opencode", Model: "dev:auto", Profile: "dev", Task: standardBrief}
	before, err := ResolveRun(reg, q, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cell := catalog.FitCell{Alias: "sonnet", Agent: "opencode", Match: catalog.RankOf(before.Model).Match, B: 9, At: time.Now()}
	if err := catalog.SaveFit(catalog.Fit{Schema: 1, Created: time.Now(), Cells: []catalog.FitCell{cell}}); err != nil {
		t.Fatal(err)
	}
	after, err := ResolveRun(reg, q, time.Now())
	if err != nil || after.Model != before.Model || !strings.HasPrefix(after.Note, before.Note) || !strings.Contains(after.Note, "fit 0.") || !strings.HasSuffix(after.Note, "(shadow)") {
		t.Errorf("shadow label: %q (%v), want %q unchanged plus the fit score", after.Note, err, before.Note)
	}
}
