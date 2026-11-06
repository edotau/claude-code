package router

import (
	"os"
	"testing"
	"time"
)

func TestLoadStateCacheReturnsIndependentDiscovery(t *testing.T) {
	// Given a settled quota file with an entry's discovery map.
	path := quotaFixture(t, `{"acct":{"discovery":{"agent|model":"2026-11-01T00:00:00Z"}}}`)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	state := LoadState()
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if got := state["acct"].Discovery["agent|model"]; !got.Equal(want) {
		t.Fatalf("initial Discovery[agent|model] = %v, want %v", got, want)
	}
	// When a caller changes an existing discovery value and adds another.
	state["acct"].Discovery["agent|model"] = want.Add(time.Hour)
	state["acct"].Discovery["caller-only"] = want
	next := LoadState()
	// Then the next LoadState retains the original discovery map.
	if got := next["acct"].Discovery["agent|model"]; !got.Equal(want) {
		t.Errorf("Discovery[agent|model] = %v, want %v", got, want)
	}
	if _, ok := next["acct"].Discovery["caller-only"]; ok {
		t.Error("Discovery contains caller-only mutation")
	}
}
