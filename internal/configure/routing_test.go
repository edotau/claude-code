package configure

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// fieldByKey finds a registered field by its Key across every module, failing the test if absent.
func fieldByKey(t *testing.T, key string) RoutingField {
	t.Helper()
	for _, m := range Modules() {
		for _, f := range m.Fields() {
			if f.Key == key {
				return f
			}
		}
	}
	t.Fatalf("no field named %q", key)
	return RoutingField{}
}

// TestWeightsPriceRoundTripsAndResets checks Set/Get agree, headroom always picks up the remainder (the
// three weights still sum to 1), and "" resets the field (removing the overlay's own weights).
func TestWeightsPriceRoundTripsAndResets(t *testing.T) {
	f := fieldByKey(t, "weights.price")
	var r providers.Routing
	if err := f.Set(&r, "0.3"); err != nil {
		t.Fatal(err)
	}
	if got := f.Get(r); got != "0.3" {
		t.Errorf("Get() = %q, want 0.3", got)
	}
	if sum := r.Weights["price"] + r.Weights["headroom"] + r.Weights["latency"]; sum != 1 {
		t.Errorf("weights sum = %v, want 1 (price=%v headroom=%v)", sum, r.Weights["price"], r.Weights["headroom"])
	}
	if err := f.Set(&r, ""); err != nil {
		t.Fatal(err)
	}
	if r.Weights != nil {
		t.Errorf("reset must clear Weights entirely, got %v", r.Weights)
	}
}

// TestWeightsPriceRejectsOutOfRange checks Set refuses a value outside [0,1].
func TestWeightsPriceRejectsOutOfRange(t *testing.T) {
	f := fieldByKey(t, "weights.price")
	var r providers.Routing
	for _, bad := range []string{"1.5", "-0.1", "not-a-number", "NaN"} {
		if err := f.Set(&r, bad); err == nil {
			t.Errorf("weights.price=%q: want an error", bad)
		}
	}
}

// TestLadderRoundTripsAndResets checks the opencode ladder field's comma-separated Set/Get and "" reset.
func TestLadderRoundTripsAndResets(t *testing.T) {
	f := fieldByKey(t, "opencode.ladder")
	var r providers.Routing
	if err := f.Set(&r, "openai, opencode-go"); err != nil {
		t.Fatal(err)
	}
	if got := f.Get(r); got != "openai,opencode-go" {
		t.Errorf("Get() = %q, want openai,opencode-go", got)
	}
	if err := f.Set(&r, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.Get(r); got != "" {
		t.Errorf("Get() after reset = %q, want empty", got)
	}
}

// TestWorkRepoMarkersRoundTripsAndResets checks the markers field's Set/Get and that an explicit "" reset
// clears the overlay field back to unset (not an explicitly-empty list).
func TestWorkRepoMarkersRoundTripsAndResets(t *testing.T) {
	f := fieldByKey(t, "opencode.workRepoMarkers")
	var r providers.Routing
	if err := f.Set(&r, "git.corp.example, corp-widgets"); err != nil {
		t.Fatal(err)
	}
	if got := f.Get(r); got != "git.corp.example,corp-widgets" {
		t.Errorf("Get() = %q, want git.corp.example,corp-widgets", got)
	}
	if err := f.Set(&r, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.Get(r), "none") {
		t.Errorf("Get() after reset = %q, want it to read as no markers configured", f.Get(r))
	}
}

// TestFreeTiersRoundTripsEnumValuesAndRejectsOthers checks the free-tiers enum Set/Get for each allowed
// value and that an unrecognized value is rejected.
func TestFreeTiersRoundTripsEnumValuesAndRejectsOthers(t *testing.T) {
	f := fieldByKey(t, "opencode.freeTiers")
	for _, want := range []string{"sonnet,haiku", "haiku", "none"} {
		var r providers.Routing
		if err := f.Set(&r, want); err != nil {
			t.Fatalf("Set(%q): %v", want, err)
		}
		if got := f.Get(r); got != want {
			t.Errorf("Get() after Set(%q) = %q, want %q", want, got, want)
		}
	}
	var r providers.Routing
	if err := f.Set(&r, "opus"); err == nil {
		t.Error(`Set("opus") must fail: opus is never eligible for zen-free`)
	}
}

func TestFreeTiersSonnetOnly(t *testing.T) {
	f := fieldByKey(t, "opencode.freeTiers")
	tiers := []string{"sonnet"}
	var r providers.Routing
	r.Opencode.FreeTiers = &tiers
	if got := f.Get(r); got != "sonnet" {
		t.Fatalf("Get(sonnet-only routing) = %q, want sonnet", got)
	}
	if !slices.Contains(f.Values, "sonnet") {
		t.Fatal("free-tier choices must include sonnet")
	}
	var updated providers.Routing
	if err := f.Set(&updated, "sonnet"); err != nil {
		t.Fatalf("Set(sonnet): %v", err)
	}
	if updated.Opencode.FreeTiers == nil || !slices.Equal(*updated.Opencode.FreeTiers, tiers) {
		t.Fatalf("Set(sonnet) must store only sonnet, got %v", updated.Opencode.FreeTiers)
	}
	if got := f.Get(updated); got != "sonnet" {
		t.Errorf("Get() after Set(sonnet) = %q, want sonnet", got)
	}
}

// TestFreeFirstOutputRoundTripsAndRejectsBadDuration checks the duration field's Set/Get and "" reset,
// and that a non-positive or unparseable duration is rejected.
func TestFreeFirstOutputRoundTripsAndRejectsBadDuration(t *testing.T) {
	f := fieldByKey(t, "opencode.freeFirstOutput")
	var r providers.Routing
	if err := f.Set(&r, "45s"); err != nil {
		t.Fatal(err)
	}
	if got := f.Get(r); got != "45s" {
		t.Errorf("Get() = %q, want 45s", got)
	}
	if err := f.Set(&r, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.Get(r); got != "1m30s" {
		t.Errorf("Get() after reset = %q, want the 90s floor", got)
	}
	for _, bad := range []string{"not-a-duration", "-5s", "0s"} {
		var scratch providers.Routing
		if err := f.Set(&scratch, bad); err == nil {
			t.Errorf("Set(%q): want an error", bad)
		}
	}
}

func TestWeightsPriceKeepsLatency(t *testing.T) {
	f := fieldByKey(t, "weights.price")
	r := providers.Routing{Weights: map[string]float64{"price": .3, "headroom": .5, "latency": .2}}
	if err := f.Set(&r, "0.4"); err != nil {
		t.Fatal(err)
	}
	if r.Weights["latency"] != .2 || math.Abs(r.Weights["headroom"]-.4) > 1e-12 {
		t.Fatal(r.Weights)
	}
	if err := f.Set(&r, "0.9"); err == nil {
		t.Fatal("accepted price above remaining budget")
	}
}

func TestWeightsLatencyRescales(t *testing.T) {
	f := fieldByKey(t, "weights.latency")
	r := providers.Routing{Weights: map[string]float64{"price": .25, "headroom": .75}}
	for _, l := range []string{"0.6", "1", "0", ""} {
		if err := f.Set(&r, l); err != nil {
			t.Fatal(err)
		}
		if l == "" {
			if r.Weights != nil {
				t.Fatal(r.Weights)
			}
			continue
		}
		if sum := r.Weights["price"] + r.Weights["headroom"] + r.Weights["latency"]; math.Abs(sum-1) > 1e-12 {
			t.Fatal(r.Weights)
		}
		if l == "0.6" && math.Abs(r.Weights["price"]-.1) > 1e-12 {
			t.Fatal(r.Weights)
		}
	}
	for _, bad := range []string{"NaN", "+Inf", "-Inf", "-0.1", "1.1", "bad"} {
		if err := f.Set(&r, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestSpendCapField(t *testing.T) {
	for _, key := range []string{"spendCap.sessionUSD", "spendCap.dayUSD"} {
		f := fieldByKey(t, key)
		var r providers.Routing
		if f.Get(r) != "off" || f.IsSet(r) {
			t.Fatal("unset cap")
		}
		for _, value := range []string{"5", "1000000", "0.001"} {
			if err := f.Set(&r, value); err != nil {
				t.Fatal(err)
			}
			if !f.IsSet(r) {
				t.Fatal("not set")
			}
		}
		for _, bad := range []string{"0", "-1", "1000001", "NaN", "+Inf", "-Inf", "bad"} {
			if err := f.Set(&r, bad); err == nil {
				t.Errorf("accepted %q", bad)
			}
		}
		if err := f.Set(&r, "5"); err != nil || f.Get(r) != "5.00" {
			t.Fatalf("Get=%s err=%v", f.Get(r), err)
		}
		if err := f.Set(&r, ""); err != nil || f.IsSet(r) || f.Get(r) != "off" {
			t.Fatal("reset failed")
		}
	}
}
