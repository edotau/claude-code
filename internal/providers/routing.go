package providers

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/vault"
)

// EnvRouteSubagents opts subagent routing out for the session ("0" = off, anything else = on); it beats
// the vault routing.subagents field, which beats the default (on).
const EnvRouteSubagents = "HARNESS_ROUTE_SUBAGENTS"

// firstOutputFloor is FirstOutput's last-resort value when FreeFirstOutput is empty or fails to parse;
// every other field reads the merged routing object directly — defaults.json seeds the real defaults.
const firstOutputFloor = 90 * time.Second

// Routing is the one `routing` object in the providers overlay: weights, cell overrides, the subagent
// offload switch and opencode's own sub-pick knobs. It replaces the registry's former top-level
// RouteWeights, Route and OpencodeLadder fields.
type Routing struct {
	// Weights replaces internal/route's composite-score weights whole (was top-level routeWeights).
	Weights map[string]float64 `json:"weights,omitempty"`
	// Cells overrides internal/routetable's table cells, keyed runner → tier (was top-level route).
	Cells map[string]map[string]RouteCell `json:"cells,omitempty"`
	// Subagents toggles the agent-effort pressure hook's coworker offload; nil = on.
	Subagents *bool `json:"subagents,omitempty"`
	// ServedAware lets the router pick Databricks models the workspace lists READY; nil = off (static menu).
	ServedAware *bool           `json:"servedAware,omitempty"`
	Opencode    OpencodeRouting `json:"opencode,omitzero"`
}

// OpencodeRouting is opencode's own sub-pick knobs: the paid-backend ladder order, the work-repo markers
// that block zen-free, which tiers zen-free may serve, and the free leg's stall deadline. Ladder,
// WorkRepoMarkers and FreeTiers are pointers: nil means unset (inherit the default), a pointer to an
// empty slice means explicitly empty — the distinction `omitempty` on a plain slice cannot survive a
// marshal round-trip (an empty slice and an absent field both omit).
type OpencodeRouting struct {
	// Ladder orders opencode's own paid backends (RouteOpencodeSubIDs); the free Zen catalog always runs
	// first regardless of this order (was top-level opencodeLadder).
	Ladder *[]string `json:"ladder,omitempty"`
	// WorkRepoMarkers are the remote-URL substrings marking a repo as work (zen-free never serves one);
	// defaults.json seeds an empty list — no repo is a work repo until the user sets markers.
	WorkRepoMarkers *[]string `json:"workRepoMarkers,omitempty"`
	// FreeTiers are the tiers zen-free may serve; defaults.json seeds sonnet+haiku. "opus" is rejected by Validate.
	FreeTiers *[]string `json:"freeTiers,omitempty"`
	// FreeFirstOutput is the free leg's stall deadline, a Go duration string; defaults.json seeds "90s".
	FreeFirstOutput string `json:"freeFirstOutput,omitempty"`
}

// LadderOrder returns the configured paid-backend ladder order (merged registry field; defaults.json
// seeds it); nil (unset) reads as no paid backends.
func (o OpencodeRouting) LadderOrder() []string {
	if o.Ladder == nil {
		return nil
	}
	return *o.Ladder
}

// Markers returns the work-repo markers to check (merged registry field; defaults.json seeds it).
func (o OpencodeRouting) Markers() []string {
	if o.WorkRepoMarkers == nil {
		return nil
	}
	return *o.WorkRepoMarkers
}

// FreeTierOK reports whether zen-free may serve tier (merged registry field; defaults.json seeds it).
func (o OpencodeRouting) FreeTierOK(tier string) bool {
	if o.FreeTiers == nil {
		return false
	}
	return slices.Contains(*o.FreeTiers, tier)
}

// FirstOutput is the free leg's stall deadline: the merged registry field when it parses to a positive
// duration, else firstOutputFloor — the only case Validate does not already reject is an unset field.
func (o OpencodeRouting) FirstOutput() time.Duration {
	if d, err := time.ParseDuration(o.FreeFirstOutput); err == nil && d > 0 {
		return d
	}
	return firstOutputFloor
}

// SubagentsOn reports whether subagent offload routing is on: nil (unset) defaults to on.
func (r Routing) SubagentsOn() bool {
	return r.Subagents == nil || *r.Subagents
}

// ServedAwareOn reports whether served-model-aware routing is on: nil (unset) defaults to off.
func (r Routing) ServedAwareOn() bool {
	return r.ServedAware != nil && *r.ServedAware
}

// ServedRoutingOn reads the overlay's servedAware switch (defaults.json never sets it); a read failure reads as off.
func ServedRoutingOn() bool {
	ov, err := RoutingOverlay()
	return err == nil && ov.ServedAwareOn()
}

// DefaultRouting is defaults.json's own routing object, with no overlay applied — lane B labels a resolved
// value "default" or "vault" by comparing against it.
func DefaultRouting() Routing {
	var base Registry
	if err := json.Unmarshal(defaultsJSON, &base); err != nil {
		return Routing{} // defaults.json always parses; a failure here is a build break, not a runtime one
	}
	return base.Routing
}

// RouteSubagentsOn resolves whether subagent offload routing is on: env HARNESS_ROUTE_SUBAGENTS ("0" = off,
// anything else = on) beats the vault overlay's own routing.subagents field, which beats the default (on).
// source is "env" when the env var is set, "vault" when the overlay itself sets subagents, else "default".
func RouteSubagentsOn() (on bool, source string) {
	if v := strings.TrimSpace(os.Getenv(EnvRouteSubagents)); v != "" {
		return v != "0", "env"
	}
	reg, err := Load()
	if err != nil {
		return true, "default"
	}
	if ov, err := RoutingOverlay(); err == nil && ov.Subagents != nil {
		return reg.Routing.SubagentsOn(), "vault"
	}
	return reg.Routing.SubagentsOn(), "default"
}

// validateRouting checks a routing object's shape: weight names/sum, cell runner/tier/account names, and
// opencode's own freeTiers/freeFirstOutput.
func validateRouting(r Routing) []string {
	var errs []string
	for runner, tiers := range r.Cells {
		if !slices.Contains(RouteRunners, runner) {
			errs = append(errs, fmt.Sprintf("routing.cells names unknown runner %q", runner))
			continue
		}
		for tier, cell := range tiers {
			if !validRouteRow(tier) {
				errs = append(errs, fmt.Sprintf("routing.cells %s/%s names unknown tier, alias or Claude id", runner, tier))
			}
			switch {
			case cell.Account != "" && !validRouteAccountKey(cell.Account):
				errs = append(errs, fmt.Sprintf("routing.cells %s/%s names account_key %q with no known billing entry", runner, tier, cell.Account))
			case cell.Account == "" && runner == "opencode" && cell.Model != "" &&
				!slices.Contains(RouteOpencodeModelPrefixes, routeModelPrefix(cell.Model)):
				errs = append(errs, fmt.Sprintf("routing.cells opencode/%s model %q prefix has no known billing account; set account_key explicitly", tier, cell.Model))
			}
		}
	}
	if err := ValidateRouteWeights(r.Weights); err != nil {
		errs = append(errs, err.Error())
	}
	for _, n := range r.Opencode.LadderOrder() {
		if !slices.Contains(RouteOpencodeSubIDs, n) {
			errs = append(errs, fmt.Sprintf("routing.opencode.ladder names unknown opencode sub id %q (%s)", n, strings.Join(RouteOpencodeSubIDs, ", ")))
		}
	}
	if r.Opencode.FreeTierOK("opus") {
		errs = append(errs, "routing.opencode.freeTiers cannot include \"opus\"")
	}
	if r.Opencode.FreeFirstOutput != "" {
		if d, err := time.ParseDuration(r.Opencode.FreeFirstOutput); err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("routing.opencode.freeFirstOutput %q must be a positive Go duration", r.Opencode.FreeFirstOutput))
		}
	}
	return errs
}

// validRouteRow reports whether key names a routed row: a tier, an alias, or a canonical Claude id of a routed tier.
func validRouteRow(key string) bool {
	if slices.Contains(RouteTiers, key) {
		return true
	}
	canonical, _, ok := models.Resolve(key)
	return ok && slices.Contains(RouteTiers, models.FamilyOf(canonical))
}

// NormalizeOpencodeLadder maps each routing.opencode.ladder entry that names a registry provider (e.g. a
// legacy "chatgpt") to that provider's own OpencodeID (e.g. "openai"), so the ladder always carries sub
// ids; an entry that is already a sub id, or names nothing in r.Providers, passes through unchanged
// (Validate catches a truly unknown id). Duplicates after mapping are dropped, first occurrence wins.
// Called from Parse before Validate, so a legacy provider-name entry normalizes before it is checked.
func NormalizeOpencodeLadder(r *Registry) *[]string {
	ladder := r.Routing.Opencode.LadderOrder()
	if ladder == nil {
		return nil
	}
	seen := make(map[string]bool, len(ladder))
	out := make([]string, 0, len(ladder))
	for _, id := range ladder {
		if p, ok := r.Providers[id]; ok && p.OpencodeID != "" {
			id = p.OpencodeID
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return &out
}

// UpdateRouting reads the providers overlay's routing object, applies fn to it, validates the merged
// registry, and writes it back, keeping the previous generation as overlay/providers.prev. Mirrors
// updateProviderFields in overlay.go.
func UpdateRouting(fn func(*Routing) error) error {
	return vault.Update(func(tx *vault.Tx) error {
		old, _ := tx.Get(vault.Overlay, "providers")
		doc := map[string]json.RawMessage{}
		if len(old) > 0 {
			if err := json.Unmarshal([]byte(old), &doc); err != nil {
				return fmt.Errorf("vault overlay/providers: %w", err)
			}
		}
		if doc == nil {
			doc = map[string]json.RawMessage{}
		}
		var r Routing
		if b, ok := doc["routing"]; ok {
			if err := json.Unmarshal(b, &r); err != nil {
				return fmt.Errorf("vault overlay/providers: routing: %w", err)
			}
		}
		if err := fn(&r); err != nil {
			return err
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		doc["routing"] = b
		newDoc, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		newDoc = append(newDoc, '\n')
		if err := Validate(newDoc); err != nil {
			return err
		}
		if err := tx.Put(vault.Overlay, "providers.prev", old); err != nil {
			return err
		}
		return tx.Put(vault.Overlay, "providers", string(newDoc))
	})
}
