package providers

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/edotau/claude-code/internal/vault"
)

//go:embed defaults.json
var defaultsJSON []byte

// OverlayDoc returns vault overlay/providers, or fs.ErrNotExist when absent.
func OverlayDoc() ([]byte, error) {
	var doc []byte
	err := vault.View(func(tx *vault.Tx) error {
		raw, ok := tx.Get(vault.Overlay, "providers")
		if !ok {
			return fmt.Errorf("vault overlay/providers: %w", fs.ErrNotExist)
		}
		doc = []byte(raw)
		return nil
	})
	return doc, err
}

// RoutingOverlay returns the vault overlay's own routing object, unmerged with defaults.json: a nil
// field means the overlay does not set it (provenance reads "default"); defaults.json always sets a
// value once merged, so this is the only way to tell "the overlay set this" from "defaults.json did".
// A missing overlay or routing key, or any load error, returns a zero Routing (every field unset).
func RoutingOverlay() (Routing, error) {
	doc, err := OverlayDoc()
	if errors.Is(err, fs.ErrNotExist) {
		return Routing{}, nil
	}
	if err != nil {
		return Routing{}, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		return Routing{}, fmt.Errorf("vault overlay/providers: %w", err)
	}
	raw, ok := top["routing"]
	if !ok {
		return Routing{}, nil
	}
	var r Routing
	if err := json.Unmarshal(raw, &r); err != nil {
		return Routing{}, fmt.Errorf("vault overlay/providers: routing: %w", err)
	}
	return r, nil
}

// Load merges the embedded defaults with the user overlay and validates the result.
func Load() (*Registry, error) {
	doc, err := OverlayDoc()
	if errors.Is(err, fs.ErrNotExist) {
		doc = nil
	} else if err != nil {
		return nil, err
	}
	return Parse(defaultsJSON, doc)
}

// Validate checks an overlay merged with the embedded defaults.
func Validate(overlay []byte) error {
	_, err := Parse(defaultsJSON, overlay)
	return err
}

// regFile is a registry document: Registry's scalar fields plus the raw providers value (nested or flat).
type regFile struct {
	Registry
	Providers json.RawMessage `json:"providers"`
}

// decodeRegistry reads one registry document; a profile set to null maps to a nil *Provider (a removal).
func decodeRegistry(doc []byte, known map[string]string) (*Registry, error) {
	var f regFile
	if err := json.Unmarshal(doc, &f); err != nil {
		return nil, err
	}
	rows, err := decodeRows(f.Providers, known)
	if err != nil {
		return nil, err
	}
	reg := f.Registry
	reg.Providers = map[string]*Provider{}
	for n, r := range rows {
		if r.raw == nil {
			reg.Providers[n] = nil
			continue
		}
		p := &Provider{}
		if err := json.Unmarshal(r.raw, p); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		p.Provider = r.provider
		reg.Providers[n] = p
	}
	return &reg, nil
}

// Parse merges base with an optional overlay document; it always returns a non-nil Registry (even one
// Validate rejects), so a caller that discards the error (routetable's "a broken registry still routes
// on the embedded table" resilience) still has a usable reg to fall back from.
func Parse(base, overlay []byte) (*Registry, error) {
	reg, err := decodeRegistry(base, nil)
	if err != nil {
		return &Registry{Providers: map[string]*Provider{}}, fmt.Errorf("providers defaults: %w", err)
	}
	known := map[string]string{}
	for n, p := range reg.Providers {
		known[n] = p.Provider
	}
	legacyErr := checkLegacyRoutingKeys(overlay)
	if len(overlay) > 0 {
		o, err := decodeRegistry(overlay, known)
		if err != nil {
			return reg, fmt.Errorf("vault overlay/providers: %w", err)
		}
		if err := applyOverlay(reg, o); err != nil {
			legacyErr = cmp.Or(legacyErr, fmt.Errorf("vault overlay/providers: %w", err))
		}
	}
	for n, p := range reg.Providers {
		p.Name = n
	}
	reg.order = declaredOrder(reg, base, overlay)
	// Load-only: derived preset fields are never written back to the overlay.
	expandPresets(reg)
	decorateModels(reg)
	reg.Routing.Opencode.Ladder = NormalizeOpencodeLadder(reg) // legacy provider-name ladder entries, before Validate
	if legacyErr != nil {
		return reg, legacyErr
	}
	return reg, reg.Validate()
}

// legacyRoutingKeys maps a retired top-level overlay key to the routing path it moved under; Parse rejects
// an overlay that still carries one (a guard, not an alias — the live vault was migrated once).
var legacyRoutingKeys = map[string]string{
	"route":          "routing.cells",
	"routeWeights":   "routing.weights",
	"opencodeLadder": "routing.opencode.ladder",
}

// checkLegacyRoutingKeys rejects an overlay document that still carries a retired top-level routing key;
// an empty overlay (no document at all) has none.
func checkLegacyRoutingKeys(overlay []byte) error {
	if len(overlay) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(overlay, &top); err != nil {
		return fmt.Errorf("vault overlay/providers: %w", err)
	}
	for _, key := range []string{"route", "routeWeights", "opencodeLadder"} {
		if _, ok := top[key]; ok {
			return fmt.Errorf("vault overlay/providers: %q moved to %s", key, legacyRoutingKeys[key])
		}
	}
	return nil
}

// applyOverlay merges overlay fields o into base, cell by cell for routes and whole-value for scalars.
func applyOverlay(base, o *Registry) error {
	if o.Default != "" {
		base.Default = o.Default
	}
	if o.Fallback != nil {
		base.Fallback = o.Fallback
	}
	if o.OpencodePlugins != nil {
		base.OpencodePlugins = o.OpencodePlugins
	}
	if o.AgentPlans != nil {
		base.AgentPlans = o.AgentPlans
	}
	if o.AgentPlanFormat != "" {
		base.AgentPlanFormat = o.AgentPlanFormat
	}
	applyRoutingOverlay(&base.Routing, &o.Routing)
	if base.Providers == nil {
		base.Providers = map[string]*Provider{}
	}
	var errs []error
	for _, n := range slices.Sorted(maps.Keys(o.Providers)) {
		p := o.Providers[n]
		if p == nil { // "name": null removes a built-in
			delete(base.Providers, n)
			continue
		}
		if prev, ok := base.Providers[n]; ok && prev.Provider != p.Provider {
			errs = append(errs, fmt.Errorf("profile %q is defined under both %s and %s; profile names are unique across providers",
				n, prev.Provider, p.Provider))
			continue
		}
		base.Providers[n] = p
	}
	return errors.Join(errs...)
}

// applyRoutingOverlay merges o into base: cells cell by cell (matching how routetable.LoadFrom applies
// them), every other field replaced whole when the overlay sets it.
func applyRoutingOverlay(base, o *Routing) {
	for runner, tiers := range o.Cells {
		if base.Cells == nil {
			base.Cells = map[string]map[string]RouteCell{}
		}
		if base.Cells[runner] == nil {
			base.Cells[runner] = map[string]RouteCell{}
		}
		maps.Copy(base.Cells[runner], tiers)
	}
	if o.Weights != nil {
		base.Weights = o.Weights // an overlay replaces the weights whole, per its docstring
	}
	if o.Subagents != nil {
		base.Subagents = o.Subagents
	}
	if o.Opencode.Ladder != nil {
		base.Opencode.Ladder = o.Opencode.Ladder
	}
	if o.Opencode.WorkRepoMarkers != nil {
		base.Opencode.WorkRepoMarkers = o.Opencode.WorkRepoMarkers
	}
	if o.Opencode.FreeTiers != nil {
		base.Opencode.FreeTiers = o.Opencode.FreeTiers
	}
	if o.Opencode.FreeFirstOutput != "" {
		base.Opencode.FreeFirstOutput = o.Opencode.FreeFirstOutput
	}
}
