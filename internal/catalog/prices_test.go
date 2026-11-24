package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var pricesT0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func fixtureFold(t *testing.T) (Prices, PriceReport) {
	t.Helper()
	raw, err := os.ReadFile("testdata/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	p, rep, err := FoldSnapshot(raw, pricesT0)
	if err != nil {
		t.Fatal(err)
	}
	return p, rep
}

// The fold counts prices, limits and deprecated rows, and mirrors every provider's row as the snapshot has it.
func TestFoldSnapshot(t *testing.T) {
	p, rep := fixtureFold(t)
	if rep.Models != 7 || rep.Priced != 5 || rep.Limits != 6 || rep.Deprecated != 1 {
		t.Errorf("report = %+v", rep)
	}
	if row, ok := p.Row("databricks", "databricks-claude-opus-5-5"); !ok || row.Input != 99 {
		t.Errorf("databricks row = %+v, %v", row, ok)
	}
	if h, _ := p.Lookup("claude-haiku-5-5"); len(h.Tiers) != 1 || h.Tiers[0].Over != 100000 || h.Tiers[0].CacheRead != 0.01 {
		t.Errorf("haiku tiers = %+v, want one tier inheriting cache_read", h.Tiers)
	}
}

// A model id only one provider lists resolves; one two providers list is not known; the provider's own row reads.
func TestPricesLookup(t *testing.T) {
	p, _ := fixtureFold(t)
	if r, ok := p.Rate("claude-opus-4-5"); !ok || r.Input != 5 || !p.Deprecated("claude-opus-4-5") {
		t.Errorf("unique id = %+v, %v, want the anthropic price, deprecated", r, ok)
	}
	if _, ok := p.Lookup("claude-opus-5-5"); ok {
		t.Error("an id two providers list must not resolve")
	}
	if r, ok := p.Row("anthropic", "claude-opus-5-5"); !ok || r.Input != 4 {
		t.Errorf("provider row = %+v, %v", r, ok)
	}
	if _, ok := p.Rate("free-model"); ok {
		t.Error("a row without a price must not give a rate")
	}
}

// Bad rows are rejected by quoted name and never abort the fold.
func TestFoldSnapshotRejects(t *testing.T) {
	_, rep := fixtureFold(t)
	if len(rep.Rejected) != 5 {
		t.Fatalf("rejected = %v, want negative-price, absurd-price, bad-limit, bad-shape, null-row", rep.Rejected)
	}
	for _, r := range rep.Rejected {
		if !strings.HasPrefix(r, `"other/`) {
			t.Errorf("rejection %q does not name its row", r)
		}
	}
}

// A body that is not a snapshot is an error, and nothing in it is taken.
func TestFoldSnapshotRefusesNonSnapshots(t *testing.T) {
	for _, body := range []string{`[1]`, `{"error":{}}`, `{"p":{"models":{"m":null}}}`, `{"p":{"models":{"m":{}}}}`} {
		if _, _, err := FoldSnapshot([]byte(body), pricesT0); err == nil {
			t.Errorf("%s accepted as a snapshot", body)
		}
	}
}

// prices.json round-trips; the snapshot path follows OPENCODE_MODELS_PATH; a missing file is an error.
func TestPricesFileAndSnapshotPath(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("OPENCODE_MODELS_PATH", filepath.Join(t.TempDir(), "missing.json"))
	if _, _, err := ReadSnapshot(); err == nil {
		t.Error("a missing snapshot file must be an error")
	}
	p, _ := fixtureFold(t)
	if err := SavePrices(p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPrices()
	if err != nil || !got.Deprecated("claude-opus-4-5") || !got.Read.Equal(pricesT0) {
		t.Errorf("round trip = %+v, %v", got, err)
	}
}

// A model the provider lacks reads the vendor's own row, with dots spelled as dashes; the provider's row wins.
func TestPricesListRow(t *testing.T) {
	p, _ := fixtureFold(t)
	p.Providers["openai"] = map[string]ModelPrice{"gpt-6.1-sol": {Priced: true, Input: 2}}
	if r, ok := p.ListRow("databricks", "claude-opus-5-5"); !ok || r.Input != 99 {
		t.Errorf("own row = %+v, %v, want the databricks price", r, ok)
	}
	if r, ok := p.ListRow("databricks", "claude-opus-4-5"); !ok || r.Input != 5 {
		t.Errorf("vendor row = %+v, %v, want the anthropic price", r, ok)
	}
	for _, c := range [][2]string{{"databricks", "gpt-6-1-sol"}, {"databricks", "gpt-6.1-sol"}, {"openai", "gpt-6-1-sol"}} {
		if r, ok := p.ListRow(c[0], c[1]); !ok || r.Input != 2 {
			t.Errorf("ListRow(%s, %s) = %+v, %v", c[0], c[1], r, ok)
		}
	}
	if _, ok := p.ListRow("databricks", "llama-4"); ok {
		t.Error("an unknown model must stay unpriced")
	}
}
