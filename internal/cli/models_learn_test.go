package cli

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
)

// priceScratch is a scratch config dir holding a two-day-old prices.json, with fetch stubbed to fn.
func priceScratch(t *testing.T, now time.Time, fn func(context.Context) ([]byte, error)) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := catalog.FetchSnapshot
	t.Cleanup(func() { catalog.FetchSnapshot = old })
	catalog.FetchSnapshot = fn
	prev := catalog.Prices{Schema: 1, Fetched: now.Add(-48 * time.Hour), Models: map[string]catalog.ModelPrice{"x": {Input: 1}}}
	if err := catalog.SavePrices(prev); err != nil {
		t.Fatal(err)
	}
}

// An unreachable snapshot reports nothing, leaves prices.json byte for byte, and is not retried within the hour.
func TestFoldPricesUnreachableLeavesFile(t *testing.T) {
	now, fetches := time.Now(), 0
	priceScratch(t, now, func(context.Context) ([]byte, error) { fetches++; return nil, errors.New("unreachable") })
	before, _ := os.ReadFile(catalog.PricesPath())
	for range 2 {
		if rep := foldPrices(now, false); rep != nil {
			t.Fatalf("report = %+v, want none", rep)
		}
	}
	if after, _ := os.ReadFile(catalog.PricesPath()); string(after) != string(before) {
		t.Error("prices.json changed on an unreachable snapshot")
	}
	if fetches != 1 {
		t.Errorf("%d fetches, want 1 (the retry within the hour is throttled)", fetches)
	}
}

// A reachable snapshot fills prices.json and keeps the rows it does not list.
func TestFoldPricesFills(t *testing.T) {
	now := time.Now()
	priceScratch(t, now, func(context.Context) ([]byte, error) {
		return []byte(`{"p":{"models":{"m-1":{"cost":{"input":1,"output":2},"limit":{"context":10}}}}}`), nil
	})
	if rep := foldPrices(now, false); rep == nil || rep.Priced != 1 || rep.Limits != 1 {
		t.Fatalf("report = %+v, want one priced row with a limit", rep)
	}
	if p, _ := catalog.ReadPrices(); len(p.Models) != 2 {
		t.Errorf("prices.json = %+v", p)
	}
}
