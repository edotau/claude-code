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
	p, rep, err := FoldSnapshot(raw, owner, pricesT0)
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
	if rep.Models != 4 || rep.Priced != 3 || rep.Limits != 4 || rep.Deprecated != 1 {
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
	if len(rep.Rejected) != 4 {
		t.Fatalf("rejected = %v, want negative-price, absurd-price, bad-limit, bad-shape", rep.Rejected)
	}
	for _, r := range rep.Rejected {
		if !strings.HasPrefix(r, "other/") {
			t.Errorf("rejection %q does not name its row", r)
		}
	}
}

// An owner price is never replaced: the snapshot keeps the limits and drops its price.
func TestFoldSnapshotOwnerWins(t *testing.T) {
	key := models.ModelKey("databricks-claude-opus-5-5")
	p, rep := fixtureFold(t, map[string]bool{key: true})
	if got := p.Models[key]; got.Priced || got.Input != 0 || got.Context != 1000000 {
		t.Errorf("owner-priced row = %+v, want no price and limits kept", got)
	}
	if rep.OwnerKept != 1 || rep.Priced != 2 {
		t.Errorf("report = %+v, want one owner price kept", rep)
	}
}

// Fetch failure and a non-snapshot body error out; prices.json round-trips and goes stale after the window.
func TestPricesFileAndFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, _, err := FoldSnapshot([]byte(`[1]`), nil, pricesT0); err == nil {
		t.Error("array body accepted as a snapshot")
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
