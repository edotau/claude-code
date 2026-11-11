package providers

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/vault"
)

// firstOutputFloor is FirstOutput's last-resort value when FreeFirstOutput is empty or fails to parse;
// every other field reads the merged routing object directly — defaults.json seeds the real defaults.
const firstOutputFloor = 90 * time.Second

// Routing is the one `routing` object in the providers overlay: weights, cell overrides, routing switches and
// opencode's own sub-pick knobs. It replaces the registry's former top-level
// RouteWeights, Route and OpencodeLadder fields.
type Routing struct {
	// Weights replaces internal/route's composite-score weights whole (was top-level routeWeights).
	Weights map[string]float64 `json:"weights,omitempty"`
	// Cells overrides internal/router's table cells, keyed agent → alias (was top-level route).
	Cells map[string]map[string]RouteCell `json:"cells,omitempty"`
	// ServedAware lets the router pick Databricks models the workspace lists READY; nil = off (static menu).
	ServedAware *bool `json:"servedAware,omitempty"`
	// ClaudeAnywhere scores claude into the first ladder rung it beats instead of last; nil = off.
	ClaudeAnywhere *bool `json:"claudeAnywhere,omitempty"`
	// Ladders orders the fallback agents after want, keyed by ladder name; an overlay replaces a ladder by name.
	Ladders  map[string][]Rung `json:"ladders,omitempty"`
	Opencode OpencodeRouting   `json:"opencode,omitzero"`
}

// Ladder names: default unless HARNESS_ROUTE asks (router.ladderName); firstParty and databricks:<p> narrow accounts.
const (
	LadderDefault    = "default"
	LadderFirstParty = "firstParty"
	LadderDatabricks = "databricks"
)

// ladderNames are the ladders routing.ladders may set.
var ladderNames = []string{LadderDefault, LadderFirstParty, LadderDatabricks}

// Rung is one ladder step: an agent, a provider or both; Models orders its vendor models (opencode: sub ids).
type Rung struct {
	Agent    string   `json:"agent,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Models   []string `json:"models,omitempty"`
}

// label names the rung in errors and skips: "agent/provider", "*" for an unset side.
func (g Rung) label() string { return cmp.Or(g.Agent, "*") + "/" + cmp.Or(g.Provider, "*") }

// OpencodeRouting is opencode's own sub-pick knobs: the paid-backend ladder order, the work-repo markers
// that block zen-free, which aliases zen-free may serve, and the free dispatch's stall deadline. Ladder,
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
	// FreeTiers are the aliases zen-free may serve; defaults.json seeds sonnet+haiku. "opus" is rejected by Validate.
	FreeTiers *[]string `json:"freeTiers,omitempty"`
	// FreeFirstOutput is the free dispatch's stall deadline, a Go duration string; defaults.json seeds "90s".
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

// FreeTierOK reports whether zen-free may serve alias (merged registry field; defaults.json seeds it).
func (o OpencodeRouting) FreeTierOK(alias string) bool {
	if o.FreeTiers == nil {
		return false
	}
	return slices.Contains(*o.FreeTiers, alias)
}

// FirstOutput is the free dispatch's stall deadline: the merged registry field when it parses to a positive
// duration, else firstOutputFloor — the only case Validate does not already reject is an unset field.
func (o OpencodeRouting) FirstOutput() time.Duration {
	if d, err := time.ParseDuration(o.FreeFirstOutput); err == nil && d > 0 {
		return d
	}
	return firstOutputFloor
}

// ClaudeAnywhereOn reports whether claude scores into the ladder: nil (unset) defaults to off.
func (r Routing) ClaudeAnywhereOn() bool {
	return r.ClaudeAnywhere != nil && *r.ClaudeAnywhere
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

// validateRouting checks a routing object's shape: weight names/sum, cell agent/alias/account names, and
// opencode's own freeTiers/freeFirstOutput.
func validateRouting(r Routing) []string {
	errs := validateRouteCells(r.Cells)
	if err := ValidateRouteWeights(r.Weights); err != nil {
		errs = append(errs, err.Error())
	}
	return append(errs, validateOpencodeRouting(r.Opencode)...)
}

// validateRouteCells checks each cell's agent, alias and account names.
func validateRouteCells(cells map[string]map[string]RouteCell) []string {
	var errs []string
	for _, agent := range slices.Sorted(maps.Keys(cells)) {
		aliases := cells[agent]
		if !slices.Contains(RouteAgents, agent) {
			errs = append(errs, fmt.Sprintf("routing.cells names unknown agent %q", agent))
			continue
		}
		for _, alias := range slices.Sorted(maps.Keys(aliases)) {
			cell := aliases[alias]
			if !validRouteRow(alias) {
				errs = append(errs, fmt.Sprintf("routing.cells %s/%s names unknown alias or Claude id", agent, alias))
			}
			errs = append(errs, validateCellAccount(agent, alias, cell)...)
		}
	}
	return errs
}

// validateCellAccount checks one cell's account_key, or the billing account its opencode model prefix implies.
func validateCellAccount(agent, alias string, cell RouteCell) []string {
	switch {
	case cell.Account != "" && !validRouteAccountKey(cell.Account):
		return []string{fmt.Sprintf("routing.cells %s/%s names account_key %q with no known billing entry", agent, alias, cell.Account)}
	case cell.Account == "" && agent == "opencode" && cell.Model != "" &&
		!slices.Contains(routeOpencodeModelPrefixes, routeModelPrefix(cell.Model)):
		return []string{fmt.Sprintf("routing.cells opencode/%s model %q prefix has no known billing account; set account_key explicitly", alias, cell.Model)}
	}
	return nil
}

// validateOpencodeRouting checks opencode's ladder, freeTiers and freeFirstOutput.
func validateOpencodeRouting(o OpencodeRouting) []string {
	var errs []string
	for _, n := range o.LadderOrder() {
		if !slices.Contains(RouteOpencodeSubIDs, n) {
			errs = append(errs, fmt.Sprintf("routing.opencode.ladder names unknown opencode sub id %q (%s)", n, strings.Join(RouteOpencodeSubIDs, ", ")))
		}
	}
	if o.FreeTierOK("opus") {
		errs = append(errs, "routing.opencode.freeTiers cannot include \"opus\"")
	}
	if o.FreeFirstOutput != "" {
		if d, err := time.ParseDuration(o.FreeFirstOutput); err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("routing.opencode.freeFirstOutput %q must be a positive Go duration", o.FreeFirstOutput))
		}
	}
	return errs
}

// validateLadders checks each ladder's name and rungs; an error names the ladder, the rung index and its label.
func validateLadders(r *Registry) []string {
	var errs []string
	for _, name := range slices.Sorted(maps.Keys(r.Routing.Ladders)) {
		if !slices.Contains(ladderNames, name) {
			errs = append(errs, fmt.Sprintf("routing.ladders names unknown ladder %q (%s)", name, strings.Join(ladderNames, ", ")))
			continue
		}
		seen := map[string]bool{}
		for i, g := range r.Routing.Ladders[name] {
			label := fmt.Sprintf("routing.ladders.%s[%d] %s", name, i, g.label())
			if seen[g.label()] {
				errs = append(errs, label+" duplicates an earlier rung")
				continue
			}
			seen[g.label()] = true
			errs = append(errs, validateRung(r, label, g)...)
		}
	}
	return errs
}

// validateRung checks one rung's agent, provider and models.
func validateRung(r *Registry, label string, g Rung) []string {
	if g.Agent == "" && g.Provider == "" {
		return []string{label + " names neither an agent nor a provider"}
	}
	errs := rungNameErrors(label, g)
	if len(g.Models) > 0 && g.Agent == "" {
		return append(errs, label+" lists models without an agent")
	}
	for _, m := range g.Models {
		if !rungModelOK(r, g.Agent, m) {
			errs = append(errs, fmt.Sprintf("%s model %q is no alias or model a row serves", label, m))
		}
	}
	return errs
}

// rungNameErrors checks that a rung's agent and provider, when set, are known.
func rungNameErrors(label string, g Rung) []string {
	var errs []string
	if g.Agent != "" && !slices.Contains(RouteAgents, g.Agent) {
		errs = append(errs, fmt.Sprintf("%s names unknown agent %q (%s)", label, g.Agent, strings.Join(RouteAgents, ", ")))
	}
	if g.Provider != "" && !slices.Contains(providerKeys, g.Provider) {
		errs = append(errs, fmt.Sprintf("%s names unknown provider %q (%s)", label, g.Provider, strings.Join(providerKeys, ", ")))
	}
	return errs
}

// rungModelOK: opencode takes its sub ids; any other agent an alias or a catalog model a row or the Databricks menu serves.
func rungModelOK(r *Registry, agent, m string) bool {
	if agent == "opencode" {
		return slices.Contains(RouteOpencodeSubIDs, m)
	}
	if slices.Contains(RouteAliases, m) {
		return true
	}
	id, _, ok := models.Resolve(m)
	if !ok {
		return false
	}
	if slices.ContainsFunc(DatabricksOpencodeMenu, func(e OpencodeModel) bool { return e.ID == id }) {
		return true
	}
	for _, n := range r.Names() {
		for _, v := range r.Providers[n].Models {
			if c, _, ok := models.Resolve(v); ok && c == id {
				return true
			}
		}
	}
	return false
}

// validRouteRow reports whether key names a routed row: an alias, or a canonical Claude id of a routed alias.
func validRouteRow(key string) bool {
	if slices.Contains(RouteAliases, key) {
		return true
	}
	canonical, _, ok := models.Resolve(key)
	return ok && slices.Contains(RouteAliases, models.FamilyOf(canonical))
}

// normalizeOpencodeLadder maps each routing.opencode.ladder entry that names a registry provider (e.g. a
// legacy "chatgpt") to that provider's own OpencodeID (e.g. "openai"), so the ladder always carries sub
// ids; an entry that is already a sub id, or names nothing in r.Providers, passes through unchanged
// (Validate catches a truly unknown id). Duplicates after mapping are dropped, first occurrence wins.
// Called from Parse before Validate, so a legacy provider-name entry normalizes before it is checked.
// Migration: remove after 2026-11-13 once every machine has run a post-2026-10-25 build.
func normalizeOpencodeLadder(r *Registry) *[]string {
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
		newDoc, err := routingDoc(old, fn)
		if err != nil {
			return err
		}
		if err := tx.Put(vault.Overlay, "providers.prev", old); err != nil {
			return err
		}
		return tx.Put(vault.Overlay, "providers", string(newDoc))
	})
}

// routingDoc applies fn to the routing object in the stored overlay doc old and returns the validated new doc.
func routingDoc(old string, fn func(*Routing) error) ([]byte, error) {
	doc, err := decodeOverlayTop(old)
	if err != nil {
		return nil, err
	}
	var r Routing
	if b, ok := doc["routing"]; ok {
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("vault overlay/providers: routing: %w", err)
		}
	}
	if err := fn(&r); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	doc["routing"] = b
	newDoc, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	newDoc = append(newDoc, '\n')
	if err := Validate(newDoc); err != nil {
		return nil, err
	}
	return newDoc, nil
}

// RouteWeightsSource reports "vault" when the vault overlay's own routing object sets weights, else "default".
func RouteWeightsSource() string {
	overlay, err := RoutingOverlay()
	if err != nil || overlay.Weights == nil {
		return "default"
	}
	return "vault"
}
