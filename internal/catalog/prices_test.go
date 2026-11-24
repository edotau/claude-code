package catalog

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/models"
)

var pricesT0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func fixtureFold(t *testing.T, owner map[string]bool) (Prices, PriceReport) {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	p, rep, err := FoldSnapshot(raw, owner, Prices{}, pricesT0)
	if err != nil {
		t.Fatal(err)
	}
	return p, rep
}

// Fold: prices, tiers, limits and the deprecated flag land; bad rows are rejected by name; first-party wins a key.
func TestFoldSnapshot(t *testing.T) {
	p, rep := fixtureFold(t, nil)
	opus := p.Models[models.ModelKey("claude-opus-5-5")]
	if !opus.Priced || opus.Input != 4 || opus.Context != 1000000 || opus.MaxOutput != 128000 {
		t.Errorf("opus = %+v, want the anthropic row (the databricks duplicate loses)", opus)
	}
	if rep.Models != 5 || rep.Priced != 3 || rep.Limits != 5 || rep.Deprecated != 1 {
		t.Errorf("report = %+v", rep)
	}
}

// Context tiers and the deprecated flag survive the fold.
func TestFoldSnapshotTiersAndStatus(t *testing.T) {
	p, _ := fixtureFold(t, nil)
	if h := p.Models[models.ModelKey("claude-haiku-5-5")]; len(h.Tiers) != 1 || h.Tiers[0].Over != 100000 || h.Tiers[0].Input != 0.5 {
		t.Errorf("haiku tiers = %+v", h.Tiers)
	}
	if !p.Deprecated("claude-opus-4-5") || p.Deprecated("claude-opus-5-5") {
		t.Error("deprecated flag wrong")
	}
}

// Rejections name their row and never abort the fold.
func TestFoldSnapshotRejects(t *testing.T) {
	_, rep := fixtureFold(t, nil)
	if len(rep.Rejected) != 5 {
		t.Fatalf("rejected = %v, want negative-price, absurd-price, bad-limit, bad-shape, null-row", rep.Rejected)
	}
	for _, r := range rep.Rejected {
		if !strings.HasPrefix(r, `"other/`) {
			t.Errorf("rejection %q does not name its row", r)
		}
	}
}

// An owner-priced model is counted; the file keeps the snapshot price for profiles without an override.
func TestFoldSnapshotCountsOwnerPrices(t *testing.T) {
	key := models.ModelKey("databricks-claude-opus-5-5")
	p, rep := fixtureFold(t, map[string]bool{key: true})
	if got, ok := p.Rate("claude-opus-5-5"); !ok || got.Input != 4 {
		t.Errorf("rate = %+v, %v, want the snapshot price kept", got, ok)
	}
	if rep.OwnerKept != 1 || rep.Priced != 3 {
		t.Errorf("report = %+v, want one owner-outranked price", rep)
	}
}

// A later provider fills what an earlier, emptier row lacks; a model missing from a new snapshot keeps its old row.
func TestFoldSnapshotFillsAndKeeps(t *testing.T) {
	raw := `{"a":{"models":{"m-1":{"limit":{"context":5}}}},"b":{"models":{"m-1":{"cost":{"input":1,"output":2},"limit":{"context":9}}}}}`
	prev := Prices{Models: map[string]ModelPrice{"gone": {Input: 7, Priced: true}}}
	p, rep, err := FoldSnapshot([]byte(raw), nil, prev, pricesT0)
	if err != nil {
		t.Fatal(err)
	}
	if m := p.Models["m-1"]; !m.Priced || m.Output != 2 || m.Context != 5 {
		t.Errorf("m-1 = %+v, want a's limit and b's price", m)
	}
	if !p.Models["gone"].Priced || rep.Models != 2 || rep.Priced != 2 {
		t.Errorf("kept = %+v, report = %+v", p.Models["gone"], rep)
	}
}

// An empty refreshed row keeps its old price, and a retained row's owner-outranked price is counted.
func TestFoldSnapshotRefreshKeepsAndCounts(t *testing.T) {
	prev := Prices{Models: map[string]ModelPrice{"m-1": {Priced: true, Input: 3, Context: 7}, "gone": {Priced: true, Input: 7}}}
	p, rep, err := FoldSnapshot([]byte(`{"a":{"models":{"m-1":{}}}}`), map[string]bool{"gone": true}, prev, pricesT0)
	if err != nil {
		t.Fatal(err)
	}
	if m := p.Models["m-1"]; !m.Priced || m.Input != 3 || m.Context != 7 {
		t.Errorf("m-1 = %+v, want the old price and limit kept", m)
	}
	if _, ok := p.Rate("gone"); !ok || rep.OwnerKept != 1 {
		t.Errorf("gone priced = %v, owner kept %d, want the retained price kept and counted", ok, rep.OwnerKept)
	}
}

// The originating provider's row beats a reseller that sorts earlier by name.
func TestFoldSnapshotPrefersOriginator(t *testing.T) {
	raw := `{"above":{"models":{"deepseek-x":{"cost":{"input":9,"output":9}}}},"deepseek":{"models":{"deepseek-x":{"cost":{"input":1,"output":2}}}}}`
	p, _, err := FoldSnapshot([]byte(raw), nil, Prices{}, pricesT0)
	if err != nil || p.Models["deepseek-x"].Input != 1 {
		t.Errorf("deepseek-x = %+v, %v, want the deepseek row", p.Models["deepseek-x"], err)
	}
}

// Fetch failure and a non-snapshot body error out; prices.json round-trips and goes stale after the window.
func TestPricesFileAndFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, body := range []string{`[1]`, `{"error":{}}`, `{"p":{"models":{"m":null}}}`} {
		if _, _, err := FoldSnapshot([]byte(body), nil, Prices{}, pricesT0); err == nil {
			t.Errorf("%s accepted as a snapshot", body)
		}
	}
	if _, err := FetchSnapshot(t.Context()); err == nil {
		t.Error("fetch should be off under go test")
	}
	p, _ := fixtureFold(t, nil)
	if err := SavePrices(p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPrices()
	if err != nil || !got.Deprecated("claude-opus-4-5") || !got.Fresh(pricesT0.Add(time.Hour)) || got.Fresh(pricesT0.Add(13*time.Hour)) {
		t.Errorf("round trip = %+v, %v", got, err)
	}
}
