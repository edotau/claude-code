package agent

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// fieldByKey finds a registered field by its Key across every module, failing the test if absent.
func fieldByKey(t *testing.T, key string) Field {
	t.Helper()
	for _, m := range Modules() {
		for _, f := range m.Fields() {
			if f.Key == key {
				return f
			}
		}
	}
	t.Fatalf("no field named %q", key)
	return Field{}
}

// TestWeightsPriceRoundTripsAndResets checks Set/Get agree, headroom always picks up the remainder (the
// two weights still sum to 1), and "" resets the field (removing the overlay's own weights).
func TestWeightsPriceRoundTripsAndResets(t *testing.T) {
	f := fieldByKey(t, "weights.price")
	var r providers.Routing
	if err := f.Set(&r, "0.3"); err != nil {
		t.Fatal(err)
	}
	if got := f.Get(r); got != "0.3" {
		t.Errorf("Get() = %q, want 0.3", got)
	}
	if sum := r.Weights["price"] + r.Weights["headroom"]; sum != 1 {
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
	for _, bad := range []string{"1.5", "-0.1", "not-a-number"} {
		if err := f.Set(&r, bad); err == nil {
			t.Errorf("weights.price=%q: want an error", bad)
		}
	}
}

// TestSubagentsRoundTripsAndResets checks Set/Get agree and "" resets to nil (on, the default).
func TestSubagentsRoundTripsAndResets(t *testing.T) {
	f := fieldByKey(t, "subagents")
	var r providers.Routing
	if got := f.Get(r); got != "true" {
		t.Errorf("Get() on a zero Routing = %q, want true (the default)", got)
	}
	if err := f.Set(&r, "false"); err != nil {
		t.Fatal(err)
	}
	if got := f.Get(r); got != "false" {
		t.Errorf("Get() = %q, want false", got)
	}
	if r.Subagents == nil || *r.Subagents {
		t.Errorf("Subagents = %v, want a pointer to false", r.Subagents)
	}
	if err := f.Set(&r, ""); err != nil {
		t.Fatal(err)
	}
	if r.Subagents != nil {
		t.Errorf("reset must clear Subagents to nil, got %v", r.Subagents)
	}
}

// TestCoreRoutingEnvExportsSubagentsOffOnly checks the core module's Env exports HARNESS_ROUTE_SUBAGENTS=0
// only when subagents are off; the default (on, nil) exports nothing.
func TestCoreRoutingEnvExportsSubagentsOffOnly(t *testing.T) {
	var core Module
	for _, m := range Modules() {
		if m.Name() == "routing" {
			core = m
		}
	}
	if core == nil {
		t.Fatal("no core routing module registered")
	}
	if env := core.Env(providers.Routing{}); env != nil {
		t.Errorf("Env(default) = %v, want nil (on is the default, nothing to export)", env)
	}
	off := false
	if env := core.Env(providers.Routing{Subagents: &off}); env[providers.EnvRouteSubagents] != "0" {
		t.Errorf("Env(off) = %v, want HARNESS_ROUTE_SUBAGENTS=0", env)
	}
	on := true
	if env := core.Env(providers.Routing{Subagents: &on}); env != nil {
		t.Errorf("Env(explicitly on) = %v, want nil", env)
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

// TestOpencodeModuleEnvContributesNothing checks the opencode module's Env always returns nil: it carries
// no env-shaped knob of its own, every field is read straight from the registry.
func TestOpencodeModuleEnvContributesNothing(t *testing.T) {
	var oc Module
	for _, m := range Modules() {
		if m.Name() == "opencode" {
			oc = m
		}
	}
	if oc == nil {
		t.Fatal("no opencode module registered")
	}
	if env := oc.Env(providers.Routing{}); env != nil {
		t.Errorf("Env() = %v, want nil", env)
	}
}
