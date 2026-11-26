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

// The list lookup reads the Databricks row, else the vendor's; a tiered row prices at its base tier.
func TestListPrice(t *testing.T) {
	tbl := Prices{Providers: map[string]map[string]ModelPrice{
		"databricks": {"databricks-m": {Priced: true, Input: 3}, "plain": {Priced: true, Input: 4},
			"tiered":   {Priced: true, Input: 5, Tiers: []TierPrice{{Over: 1}}},
			"unpriced": {Input: 7}},
		"anthropic": {"m": {Priced: true, Input: 9}, "claude-v": {Priced: true, Input: 6},
			"claude-unpriced": {Context: 1}},
	}}
	for _, c := range []struct {
		provider, id string
		input        float64
		ok           bool
	}{{"databricks", "m", 3, true}, {"databricks", "plain", 4, true}, {"databricks", "tiered", 5, true},
		{"databricks", "claude-v", 6, true}, {"databricks", "unpriced", 0, false},
		{"databricks", "claude-unpriced", 0, false},
		{"anthropic", "m", 0, false}, {"ultra", "m", 0, false}} {
		if p, ok := tbl.ListPrice(c.provider, c.id); ok != c.ok || (ok && p.Input != c.input) {
			t.Errorf("%s/%s = %+v, %v, want input %v ok %v", c.provider, c.id, p, ok, c.input, c.ok)
		}
	}
}

// A model the provider lacks reads the vendor's own row, with dots spelled as dashes; the provider's row wins.
func TestPricesListRow(t *testing.T) {
	p, _ := fixtureFold(t)
	p.Providers["openai"] = map[string]ModelPrice{"gpt-6.1-sol": {Priced: true, Input: 2}, "gpt-7": {Priced: true, Input: 8}, "o4-mini": {Priced: true, Input: 1},
		"gpt-8": {Priced: true, Input: 3}, "gpt-9": {Priced: true, Input: 5}}
	p.Providers["databricks"]["databricks-gpt-8"] = ModelPrice{Context: 1}
	p.Providers["databricks"]["databricks-gpt-9"] = ModelPrice{Context: 1}
	p.Providers["databricks"]["gpt-9"] = ModelPrice{Priced: true, Input: 4}
	p.Providers["databricks"]["databricks-gpt-7"] = ModelPrice{Priced: true, Input: 7}
	p.Providers["databricks"]["gpt-7"] = ModelPrice{Priced: true, Input: 6}
	for _, c := range []struct {
		prov, id string
		in       float64
	}{
		{"databricks", "claude-opus-5-5", 99}, {"databricks", "claude-opus-4-5", 5}, {"databricks", "gpt-6-1-sol", 2},
		{"databricks", "gpt-6.1-sol", 2}, {"openai", "gpt-6-1-sol", 2}, {"databricks", "gpt-7", 6},
		{"databricks", "databricks-gpt-7", 7}, {"databricks", "o4-mini", 1},
		{"databricks", "databricks-gpt-6-1-sol", 2}, {"databricks", "gpt-8", 3}, {"databricks", "gpt-9", 4},
	} {
		if r, ok := p.ListRow(c.prov, c.id); !ok || r.Input != c.in {
			t.Errorf("ListRow(%s, %s) = %+v, %v, want input %v", c.prov, c.id, r, ok, c.in)
		}
	}
	if _, ok := p.ListRow("databricks", "llama-4"); ok {
		t.Error("an unknown model must stay unpriced")
	}
	p.Providers["databricks"]["databricks-unlisted"] = ModelPrice{Context: 1}
	if r, ok := p.ListRow("databricks", "unlisted"); !ok || r.Priced || r.Context != 1 {
		t.Errorf("unpriced row = %+v, %v; want unpriced context row", r, ok)
	}
}
