package providers

import (
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

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
	checkMergedOpencode(t, reg.Routing.Opencode)
}

// checkMergedOpencode asserts the opencode.* fields of the routing overlay above landed whole.
func checkMergedOpencode(t *testing.T, oc OpencodeRouting) {
	t.Helper()
	if ladder := oc.LadderOrder(); len(ladder) != 1 || ladder[0] != "openai" {
		t.Errorf("ladder = %v", ladder)
	}
	if m := oc.Markers(); len(m) != 1 || m[0] != "git.corp.example" {
		t.Errorf("workRepoMarkers = %v", m)
	}
	if oc.FreeTierOK("sonnet") || !oc.FreeTierOK("haiku") {
		t.Errorf("freeTiers = %v, want haiku only", oc.FreeTiers)
	}
	if oc.FirstOutput().String() != "45s" {
		t.Errorf("freeFirstOutput = %v, want 45s", oc.FirstOutput())
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

// An explicitly empty freeTiers (pointer to an empty slice) round-trips, blocks zen-free and survives unrelated edits.
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
		t.Errorf("freeTiers=none must block every alias, got FreeTiers=%v", reg.Routing.Opencode.FreeTiers)
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
		t.Errorf("freeTiers=none must still block every alias after an unrelated edit, got %v", reg.Routing.Opencode.FreeTiers)
	}
}

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

func TestValidateRouteCellsOrderIsDeterministic(t *testing.T) {
	cells := map[string]map[string]RouteCell{"zz": nil, "aa": nil, "mm": nil, "bb": nil, "cc": nil}
	want := validateRouteCells(cells)
	for range 30 {
		if got := validateRouteCells(cells); !slices.Equal(got, want) {
			t.Fatalf("error order varies: %v vs %v", got, want)
		}
	}
	if !slices.IsSorted(want) {
		t.Fatalf("errors not sorted by agent: %v", want)
	}
}

// The default ladder's first two rungs are copilot, one per provider (databricks, github), each carrying a model list.
func TestDefaultLadderSplitsCopilotRungByProvider(t *testing.T) {
	reg, err := Parse(defaultsJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	rungs := reg.Routing.Ladders["default"]
	if len(rungs) < 2 {
		t.Fatalf("default ladder has %d rungs, want >= 2", len(rungs))
	}
	for i, want := range []string{"databricks", "github"} {
		g := rungs[i]
		if g.Agent != "copilot" || g.Provider != want || len(g.Models) == 0 {
			t.Errorf("rung %d = %+v, want copilot/%s with models", i, g, want)
		}
	}
}

// TestDatabricksLadderParses: the embedded databricks ladder loads and every rung names a roster agent or a provider.
func TestDatabricksLadderParses(t *testing.T) {
	reg, err := Parse(defaultsJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	rungs := reg.Routing.Ladders[LadderDatabricks]
	if len(rungs) == 0 {
		t.Fatalf("no %s ladder in defaults.json", LadderDatabricks)
	}
	for i, g := range rungs {
		if errs := validateRung(reg, g.label(), g); len(errs) > 0 {
			t.Errorf("rung %d %s: %v", i, g.label(), errs)
		}
	}
}
