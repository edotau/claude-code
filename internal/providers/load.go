package providers

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/edotau/claude-code/internal/paths"
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

// RoutingOverlay is the overlay's routing object, unmerged with defaults.json: a nil field is unset; an error is zero.
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

// loadCache holds the merged registry per vault.db stamp; Load hands out clones so callers may mutate freely.
var loadCache paths.StampCache[*Registry]

// Load merges the embedded defaults with the user overlay and validates the result.
func Load() (*Registry, error) {
	reg, err := loadCache.Get(vault.Path(), loadRegistry)
	return reg.clone(), err
}

// loadRegistry is Load's uncached merge of the vault overlay over the embedded defaults.
func loadRegistry() (*Registry, error) {
	doc, err := OverlayDoc()
	if errors.Is(err, fs.ErrNotExist) {
		doc = nil
	} else if err != nil {
		return nil, err
	}
	return Parse(defaultsJSON, doc)
}

// Known errors unless profile is in the registry.
func (r *Registry) Known(profile string) error {
	if r.Providers[profile] == nil {
		return fmt.Errorf("unknown profile %s (see configure provider --list)", profile)
	}
	return nil
}

// clone returns a deep copy of r; nil clones to nil.
func (r *Registry) clone() *Registry {
	if r == nil {
		return nil
	}
	out := *r
	out.Fallback = slices.Clone(r.Fallback)
	out.OpencodePlugins = slices.Clone(r.OpencodePlugins)
	out.order = slices.Clone(r.order)
	out.AgentPlans = cloneMap(r.AgentPlans, slices.Clone[[]Plan])
	out.Providers = cloneMap(r.Providers, (*Provider).clone)
	out.Routing = r.Routing.clone()
	return &out
}

// clone returns a deep copy of p; nil clones to nil.
func (p *Provider) clone() *Provider {
	if p == nil {
		return nil
	}
	out := *p
	out.Routes = maps.Clone(p.Routes)
	out.Models = maps.Clone(p.Models)
	out.Headers = maps.Clone(p.Headers)
	out.Env = maps.Clone(p.Env)
	return &out
}

// clone returns a deep copy of o: every map, slice and pointer it holds is fresh.
func (o Routing) clone() Routing {
	out := o
	out.Weights = maps.Clone(o.Weights)
	out.Cells = cloneMap(o.Cells, maps.Clone[map[string]RouteCell])
	out.ServedAware = clonePtr(o.ServedAware)
	out.ClaudeAnywhere = clonePtr(o.ClaudeAnywhere)
	out.Ladders = cloneMap(o.Ladders, cloneRungs)
	out.Opencode = o.Opencode.clone()
	return out
}

// cloneRungs copies a ladder's rungs, including each rung's model list.
func cloneRungs(rungs []Rung) []Rung {
	out := slices.Clone(rungs)
	for i := range out {
		out[i].Models = slices.Clone(out[i].Models)
	}
	return out
}

// clone returns a deep copy of o's pointer-to-slice knobs.
func (o OpencodeRouting) clone() OpencodeRouting {
	out := o
	out.Ladder = clonePtrSlice(o.Ladder)
	out.WorkRepoMarkers = clonePtrSlice(o.WorkRepoMarkers)
	out.FreeTiers = clonePtrSlice(o.FreeTiers)
	return out
}

// clonePtrSlice returns a pointer to a copy of *p; nil stays nil, an explicit empty stays explicit.
func clonePtrSlice(p *[]string) *[]string {
	if p == nil {
		return nil
	}
	s := slices.Clone(*p)
	return &s
}

// cloneMap returns a copy of m with each value replaced by f(value); nil stays nil.
func cloneMap[K comparable, V any](m map[K]V, f func(V) V) map[K]V {
	if m == nil {
		return nil
	}
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = f(v)
	}
	return out
}

// clonePtr returns a pointer to a copy of *p; nil stays nil.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Validate checks an overlay merged with the embedded defaults; unlike Load it refuses an unknown routing key.
func Validate(overlay []byte) error {
	if _, err := Parse(defaultsJSON, overlay); err != nil {
		return err
	}
	return checkRoutingKeys(overlay)
}

// checkRoutingKeys refuses an overlay whose routing object carries a key Routing does not define (a typo).
func checkRoutingKeys(overlay []byte) error {
	top, err := decodeOverlayTop(string(overlay))
	if err != nil {
		return err
	}
	raw, ok := top["routing"]
	if !ok {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&Routing{}); err != nil {
		return fmt.Errorf("vault overlay/providers: routing: %w", err)
	}
	return nil
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

// Parse merges base with an optional overlay; the Registry is never nil, even on an error, so callers can fall back.
func Parse(base, overlay []byte) (*Registry, error) {
	reg, err := decodeRegistry(base, nil)
	if err != nil {
		return &Registry{Providers: map[string]*Provider{}}, fmt.Errorf("providers defaults: %w", err)
	}
	mergeErr := mergeOverlay(reg, overlay)
	for n, p := range reg.Providers {
		p.Name = n
	}
	reg.order = declaredOrder(reg, base, overlay)
	// Load-only: derived preset fields are never written back to the overlay.
	expandPresets(reg)
	decorateModels(reg)
	if mergeErr != nil {
		return reg, mergeErr
	}
	return reg, reg.validate()
}

// mergeOverlay applies overlay onto reg; on an error reg keeps the base, still named, ordered and expanded by Parse.
func mergeOverlay(reg *Registry, overlay []byte) error {
	if len(overlay) == 0 {
		return nil
	}
	known := map[string]string{}
	for n, p := range reg.Providers {
		known[n] = p.Provider
	}
	o, err := decodeRegistry(overlay, known)
	if err == nil {
		err = applyOverlay(reg, o)
	}
	if err != nil {
		return fmt.Errorf("vault overlay/providers: %w", err)
	}
	return nil
}

// applyOverlay merges overlay fields o into base, cell by cell for routes and whole-value for scalars.
func applyOverlay(base, o *Registry) error {
	if err := applyProviderOverlay(base, o); err != nil { // first, so a refused overlay leaves base untouched
		return err
	}
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
	return nil
}

// applyProviderOverlay merges o's profiles into base; a null profile removes a built-in.
func applyProviderOverlay(base, o *Registry) error {
	if base.Providers == nil {
		base.Providers = map[string]*Provider{}
	}
	var errs []error
	for _, n := range slices.Sorted(maps.Keys(o.Providers)) {
		if p, prev := o.Providers[n], base.Providers[n]; p != nil && prev != nil && prev.Provider != p.Provider {
			errs = append(errs, fmt.Errorf("profile %q is defined under both %s and %s; profile names are unique across providers",
				n, prev.Provider, p.Provider))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	for n, p := range o.Providers {
		if p == nil { // "name": null removes a built-in
			delete(base.Providers, n)
			continue
		}
		base.Providers[n] = p
	}
	return nil
}

// applyRoutingOverlay merges o into base: cells cell by cell (matching how router.LoadFrom applies
// them), every other field replaced whole when the overlay sets it.
func applyRoutingOverlay(base, o *Routing) {
	mergeCells(base, o)
	if o.Weights != nil {
		base.Weights = o.Weights // an overlay replaces the weights whole, per its docstring
	}
	if o.ServedAware != nil {
		base.ServedAware = o.ServedAware
	}
	if o.ClaudeAnywhere != nil {
		base.ClaudeAnywhere = o.ClaudeAnywhere
	}
	for name, ladder := range o.Ladders {
		if base.Ladders == nil {
			base.Ladders = map[string][]Rung{}
		}
		base.Ladders[name] = ladder // a ladder is one owned list: replaced whole by name
	}
	applyOpencodeOverlay(&base.Opencode, &o.Opencode)
}

// mergeCells copies o's route cells into base cell by cell.
func mergeCells(base, o *Routing) {
	for agent, aliases := range o.Cells {
		if base.Cells == nil {
			base.Cells = map[string]map[string]RouteCell{}
		}
		if base.Cells[agent] == nil {
			base.Cells[agent] = map[string]RouteCell{}
		}
		maps.Copy(base.Cells[agent], aliases)
	}
}

// applyOpencodeOverlay replaces each opencode field whole when the overlay sets it.
func applyOpencodeOverlay(base, o *OpencodeRouting) {
	if o.Ladder != nil {
		base.Ladder = o.Ladder
	}
	if o.WorkRepoMarkers != nil {
		base.WorkRepoMarkers = o.WorkRepoMarkers
	}
	if o.FreeTiers != nil {
		base.FreeTiers = o.FreeTiers
	}
	if o.FreeFirstOutput != "" {
		base.FreeFirstOutput = o.FreeFirstOutput
	}
}

// ProfileNames lists the provider names straight off disk (no network), for tab completion.
func ProfileNames() []string {
	reg, err := Load()
	if err != nil {
		return nil
	}
	return reg.Names()
}
