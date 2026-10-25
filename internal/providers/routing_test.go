package providers

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestRoutingOverlayMergesWeightsCellsAndOpencode checks a `routing` overlay object merges its weights,
// cells and opencode.* fields onto the defaults, cell by cell for Cells, whole-value for the rest.
func TestRoutingOverlayMergesWeightsCellsAndOpencode(t *testing.T) {
	overlay := []byte(`{"routing": {
		"weights": {"price": 0.3, "headroom": 0.7},
		"cells": {"opencode": {"sonnet": {"model": "opencode-go/y"}}},
		"opencode": {"ladder": ["openai"], "workRepoMarkers": ["git.corp.example"], "freeTiers": ["haiku"], "freeFirstOutput": "45s"}
	}}`)
	reg, err := Parse(defaultsJSON, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Routing.Weights["price"] != 0.3 || reg.Routing.Weights["headroom"] != 0.7 {
		t.Errorf("weights = %v", reg.Routing.Weights)
	}
	if c := reg.Routing.Cells["opencode"]["sonnet"]; c.Model != "opencode-go/y" {
		t.Errorf("cells = %+v", c)
	}
	if ladder := reg.Routing.Opencode.LadderOrder(); len(ladder) != 1 || ladder[0] != "openai" {
		t.Errorf("ladder = %v", ladder)
	}
	if m := reg.Routing.Opencode.Markers(); len(m) != 1 || m[0] != "git.corp.example" {
		t.Errorf("workRepoMarkers = %v", m)
	}
	if reg.Routing.Opencode.FreeTierOK("sonnet") || !reg.Routing.Opencode.FreeTierOK("haiku") {
		t.Errorf("freeTiers = %v, want haiku only", reg.Routing.Opencode.FreeTiers)
	}
	if reg.Routing.Opencode.FirstOutput().String() != "45s" {
		t.Errorf("freeFirstOutput = %v, want 45s", reg.Routing.Opencode.FirstOutput())
	}
}

// TestLegacyRoutingKeysRejected checks each retired top-level key (route, routeWeights, opencodeLadder)
// fails Parse with a "moved to routing.X" error, rather than silently applying or being ignored.
func TestLegacyRoutingKeysRejected(t *testing.T) {
	cases := map[string]string{
		`{"route": {"opencode": {"sonnet": {"model": "m"}}}}`: "routing.cells",
		`{"routeWeights": {"price": 0.5, "headroom": 0.5}}`:   "routing.weights",
		`{"opencodeLadder": ["openai"]}`:                      "routing.opencode.ladder",
	}
	for overlay, wantPath := range cases {
		_, err := Parse(defaultsJSON, []byte(overlay))
		if err == nil {
			t.Errorf("overlay %s: want a legacy-key error, got nil", overlay)
			continue
		}
		if !strings.Contains(err.Error(), wantPath) {
			t.Errorf("overlay %s: error = %v, want it to name %q", overlay, err, wantPath)
		}
	}
}

// TestValidateRejectsOpencodeFreeTiersOpus checks an overlay naming "opus" in freeTiers fails Validate.
func TestValidateRejectsOpencodeFreeTiersOpus(t *testing.T) {
	overlay := []byte(`{"routing": {"opencode": {"freeTiers": ["sonnet", "opus"]}}}`)
	_, err := Parse(defaultsJSON, overlay)
	if err == nil || !strings.Contains(err.Error(), `freeTiers cannot include "opus"`) {
		t.Errorf("Parse() = %v, want the freeTiers/opus rule named", err)
	}
}

// TestValidateRejectsBadFreeFirstOutput checks a non-duration or non-positive freeFirstOutput fails Validate.
func TestValidateRejectsBadFreeFirstOutput(t *testing.T) {
	for _, bad := range []string{"not-a-duration", "-5s", "0s"} {
		overlay := []byte(`{"routing": {"opencode": {"freeFirstOutput": "` + bad + `"}}}`)
		_, err := Parse(defaultsJSON, overlay)
		if err == nil || !strings.Contains(err.Error(), "freeFirstOutput") {
			t.Errorf("freeFirstOutput=%q: err = %v, want it to name the rule", bad, err)
		}
	}
}

// TestUpdateRoutingRoundTripsAndKeepsPrev checks UpdateRouting writes the merged routing object, reads
// back the same value on reload, and keeps the previous overlay generation as providers.prev.
func TestUpdateRoutingRoundTripsAndKeepsPrev(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := UpdateRouting(func(r *Routing) error {
		r.Weights = map[string]float64{"price": 0.4, "headroom": 0.6}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first, err := OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateRouting(func(r *Routing) error {
		r.Weights = map[string]float64{"price": 0.1, "headroom": 0.9}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Routing.Weights["price"] != 0.1 {
		t.Errorf("round-tripped weights = %v, want price=0.1", reg.Routing.Weights)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		prev, ok := tx.Get(vault.Overlay, "providers.prev")
		if !ok || prev != string(first) {
			t.Errorf("providers.prev = %q, present %v; want the first write's doc", prev, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFreeTiersNoneRoundTripsAndSurvivesUnrelatedEdit checks an explicitly-empty freeTiers ("none", a
// pointer to an empty slice, not nil/unset) round-trips through UpdateRouting and a reload, blocks zen-free
// at every tier, and survives an unrelated field's own UpdateRouting write (the pointer distinction from
// OpencodeRouting's doc comment: nil is unset, &[]string{} is explicitly empty).
func TestFreeTiersNoneRoundTripsAndSurvivesUnrelatedEdit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	empty := []string{}
	if err := UpdateRouting(func(r *Routing) error {
		r.Opencode.FreeTiers = &empty
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Routing.Opencode.FreeTierOK("sonnet") || reg.Routing.Opencode.FreeTierOK("haiku") {
		t.Errorf("freeTiers=none must block every tier, got FreeTiers=%v", reg.Routing.Opencode.FreeTiers)
	}
	// an unrelated write (weights only) must not revert freeTiers back to nil (unset/default).
	if err := UpdateRouting(func(r *Routing) error {
		r.Weights = map[string]float64{"price": 0.2, "headroom": 0.8}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Routing.Opencode.FreeTiers == nil {
		t.Fatal("an unrelated UpdateRouting write must not clear a previously-set explicit-empty freeTiers")
	}
	if reg.Routing.Opencode.FreeTierOK("sonnet") || reg.Routing.Opencode.FreeTierOK("haiku") {
		t.Errorf("freeTiers=none must still block every tier after an unrelated edit, got %v", reg.Routing.Opencode.FreeTiers)
	}
}

// TestUpdateRoutingRejectsInvalidResult checks a fn that leaves the routing object invalid (an out-of-range
// weight) fails UpdateRouting without writing the vault.
func TestUpdateRoutingRejectsInvalidResult(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	err := UpdateRouting(func(r *Routing) error {
		r.Weights = map[string]float64{"price": 2, "headroom": -1}
		return nil
	})
	if err == nil {
		t.Fatal("an out-of-range weight must fail UpdateRouting")
	}
	if _, err := OverlayDoc(); err == nil {
		t.Error("a rejected UpdateRouting must not create an overlay document")
	}
}

// TestRouteSubagentsOnPrecedence checks env beats vault beats default, each reporting its own source.
func TestRouteSubagentsOnPrecedence(t *testing.T) {
	vaulttest.Use(t)

	on, source := RouteSubagentsOn()
	if !on || source != "default" {
		t.Errorf("no overlay: on=%v source=%q, want true/default", on, source)
	}

	off := false
	if err := UpdateRouting(func(r *Routing) error { r.Subagents = &off; return nil }); err != nil {
		t.Fatal(err)
	}
	on, source = RouteSubagentsOn()
	if on || source != "vault" {
		t.Errorf("vault off: on=%v source=%q, want false/vault", on, source)
	}

	t.Setenv(EnvRouteSubagents, "1")
	on, source = RouteSubagentsOn()
	if !on || source != "env" {
		t.Errorf("env=1 over vault-off: on=%v source=%q, want true/env", on, source)
	}

	t.Setenv(EnvRouteSubagents, "0")
	on, source = RouteSubagentsOn()
	if on || source != "env" {
		t.Errorf("env=0: on=%v source=%q, want false/env", on, source)
	}
}
