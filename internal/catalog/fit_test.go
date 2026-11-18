package catalog

import (
	"math"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/models"
)

var fitT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func fitOut(at time.Time, ok bool, secs float64) Outcome {
	return Outcome{Time: at, Alias: models.Sonnet, Agent: "claude", Match: "mid-b*", OK: ok, Secs: secs}
}

// Fold: a success adds to a, a quality failure to b, evidence halves in 14 days, a second run changes nothing.
func TestFitFold(t *testing.T) {
	stubRanks(t)
	f := Fit{}.Fold([]Outcome{fitOut(fitT0, true, 10), fitOut(fitT0.Add(time.Hour), false, 0)}, fitT0.Add(2*time.Hour))
	c := f.Cells[0]
	if len(f.Cells) != 1 || math.Abs(c.A-1) > 0.01 || c.B != 1 || c.Secs < 9.9 {
		t.Fatalf("fold = %+v, want one cell a~1 b=1 secs~10", f.Cells)
	}
	again := f.Fold([]Outcome{fitOut(fitT0, true, 10), fitOut(fitT0.Add(time.Hour), false, 0)}, fitT0.Add(48*time.Hour))
	if again.Folded != f.Folded || again.Created != f.Created || len(again.Cells) != 1 || again.Cells[0] != c {
		t.Errorf("second fold changed the table: %+v -> %+v", f, again)
	}
	e := c.Eval(c.At.Add(fitHalfLife))
	if math.Abs(e.N-(c.A+c.B)/2) > 1e-9 {
		t.Errorf("n after one half-life = %v, want half of %v", e.N, c.A+c.B)
	}
}

func fitWith(t *testing.T, created time.Time, c FitCell) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	c.Alias, c.Agent, c.At = models.Sonnet, "claude", fitT0
	if err := SaveFit(Fit{Schema: fitSchema, Created: created, Cells: []FitCell{c}}); err != nil {
		t.Fatal(err)
	}
	old := fitNow
	fitNow = func() time.Time { return fitT0 }
	t.Cleanup(func() { fitNow = old })
}

func firstPick(in []Candidate) string {
	return Choose(in, "claude", "standard", false).Ranked[0].Model
}

// In shadow the pick is today's for a cell whose failures would reorder it once live; after 14 days it reorders.
func TestChooseFitShadowThenLive(t *testing.T) {
	stubRanks(t)
	in := cands("mid-a", "mid-b") // mid-b is the cheaper pick today
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got := firstPick(in); got != "mid-b" {
		t.Fatalf("no fit: %s, want mid-b", got)
	}
	bad := FitCell{Match: "mid-b*", A: 1, B: 8}
	fitWith(t, fitT0.Add(-time.Hour), bad)
	c := Choose(in, "claude", "standard", false)
	if c.Ranked[0].Model != "mid-b" || !c.Shadow || c.Ranked[0].FitNote() == "" || c.Ranked[0].lift != 0 {
		t.Errorf("shadow pick %+v, want mid-b unchanged with a shadow score", c)
	}
	fitWith(t, fitT0.Add(-15*24*time.Hour), bad)
	if got := firstPick(in); got != "mid-a" {
		t.Errorf("live pick %s, want mid-a: mid-b's evidence says it fails", got)
	}
	fitWith(t, fitT0.Add(-15*24*time.Hour), FitCell{Match: "mid-b*", A: 0, B: 3})
	if got := firstPick(in); got != "mid-b" {
		t.Errorf("a cell below the evidence threshold reordered: picked %s", got)
	}
}

// Within one lift band the faster cell leads; a clearly better fit still beats a faster one.
func TestChooseFitSpeedTiebreak(t *testing.T) {
	stubRanks(t)
	in := cands("mid-a", "mid-b")
	fitWith(t, fitT0.Add(-15*24*time.Hour), FitCell{Match: "mid-a*", A: 9, Runs: 9, Secs: 90})
	if got := firstPick(in); got != "mid-a" {
		t.Errorf("a timed cell in the same band as an untimed one picked %s, want mid-a", got)
	}
	fitWith(t, fitT0.Add(-15*24*time.Hour), FitCell{Match: "mid-a*", A: 1, B: 9, Runs: 1, Secs: 1})
	if got := firstPick(in); got != "mid-b" {
		t.Errorf("a fast but failing cell picked %s, want mid-b", got)
	}
}

func BenchmarkChooseFit(b *testing.B) {
	cands := []Candidate{
		{"claude-opus-5-5", "yes"}, {"claude-sonnet-5-5", "yes"}, {"claude-haiku-5-5", ""},
		{"databricks-gpt-5-5", "no"}, {"gemini-3-8-flash", "yes"}, {"no-such-model", "yes"},
	}
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	f := Fit{Schema: fitSchema, Created: time.Now()}
	for _, m := range []string{"claude-sonnet-5-5*", "claude-opus-5-5*", "claude-haiku-5-5*"} {
		f.Cells = append(f.Cells, FitCell{Alias: models.Sonnet, Agent: "claude", Match: m, A: 9, B: 1, Runs: 9, Secs: 90, At: time.Now()})
	}
	if err := SaveFit(f); err != nil {
		b.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond) // past the stamp cache's racy window
	b.ReportAllocs()
	for b.Loop() {
		Choose(cands, "claude", "standard", true)
	}
}
