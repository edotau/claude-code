package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/catalog"
)

func priceScratch(t *testing.T, snapshot string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "models.json")
	if snapshot != "" {
		if err := os.WriteFile(path, []byte(snapshot), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENCODE_MODELS_PATH", path)
}

func oldPrices(id string) catalog.Prices {
	return catalog.Prices{Schema: 1, Providers: map[string]map[string]catalog.ModelPrice{"p": {id: {Input: 1}}}}
}

// A missing snapshot file reports nothing and leaves prices.json byte for byte.
func TestFoldPricesMissingFileLeavesTable(t *testing.T) {
	priceScratch(t, "")
	if err := catalog.SavePrices(oldPrices("x")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(catalog.PricesPath())
	if rep := foldPrices(false); rep != nil {
		t.Fatalf("report = %+v, want none", rep)
	}
	if after, _ := os.ReadFile(catalog.PricesPath()); string(after) != string(before) {
		t.Error("prices.json changed with no snapshot file")
	}
}

// A present snapshot is mirrored: rows the file no longer lists are dropped.
func TestFoldPricesMirrorsFile(t *testing.T) {
	priceScratch(t, `{"p":{"models":{"m-1":{"cost":{"input":1,"output":2},"limit":{"context":10}}}}}`)
	if err := catalog.SavePrices(oldPrices("gone")); err != nil {
		t.Fatal(err)
	}
	if rep := foldPrices(false); rep == nil || rep.Priced != 1 || rep.Limits != 1 {
		t.Fatalf("report = %+v, want one priced row with a limit", rep)
	}
	p, _ := catalog.ReadPrices()
	if _, ok := p.Row("p", "gone"); ok || len(p.Providers["p"]) != 1 {
		t.Errorf("prices.json = %+v, want only m-1", p)
	}
}
