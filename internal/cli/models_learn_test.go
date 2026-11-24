package cli

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
)

// An unreachable snapshot reports nothing and leaves an existing prices.json byte for byte; a reachable one fills it.
func TestFoldPricesUnreachableLeavesFile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := catalog.FetchSnapshot
	t.Cleanup(func() { catalog.FetchSnapshot = old })
	now := time.Now()
	prev := catalog.Prices{Schema: 1, Fetched: now.Add(-48 * time.Hour), Models: map[string]catalog.ModelPrice{"x": {Input: 1}}}
	if err := catalog.SavePrices(prev); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(catalog.PricesPath())
	catalog.FetchSnapshot = func(context.Context) ([]byte, error) { return nil, errors.New("unreachable") }
	if rep := foldPrices(now, false); rep != nil {
		t.Fatalf("report = %+v, want none", rep)
	}
	if after, _ := os.ReadFile(catalog.PricesPath()); string(after) != string(before) {
		t.Error("prices.json changed on an unreachable snapshot")
	}
	catalog.FetchSnapshot = func(context.Context) ([]byte, error) {
		return []byte(`{"p":{"models":{"m-1":{"cost":{"input":1,"output":2},"limit":{"context":10}}}}}`), nil
	}
	if rep := foldPrices(now, false); rep == nil || rep.Priced != 1 || rep.Limits != 1 {
		t.Fatalf("report = %+v, want one priced row with a limit", rep)
	}
	if p, _ := catalog.ReadPrices(); len(p.Models) != 1 {
		t.Errorf("prices.json = %+v", p)
	}
}
