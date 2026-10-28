// Package routetable is the embedded Claude-row × runner routing grid (table.json, vault-overridable): the Table,
// Cell and Price types and Load. It sits below internal/route (which scores and picks a runner) and
// internal/vendors (which renders the opencode overlay from a cell's model), so neither of those two needs to
// import the other through this data.
package routetable

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

//go:embed table.json
var tableJSON []byte

// Tiers are the routed tiers, strongest first; fable stays Claude Code's own slot.
var Tiers = providers.RouteTiers

// Runners are the table's columns in roster order; ties in a pick's cost score keep this order.
var Runners = providers.RouteRunners

// Billing types: a plan window already paid for, per-use credits, or a free third-party endpoint.
const (
	BillSubscription = "subscription"
	BillCredits      = "credits"
	BillFree         = "free"
)

// Price is USD per 1M tokens.
type Price struct {
	In       float64 `json:"in"`
	CachedIn float64 `json:"cached_in"`
	Out      float64 `json:"out"`
}

// Cell is one runner × tier row.
type Cell struct {
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
	Account string `json:"account_key"`
	Price   Price  `json:"price"`
}

// Table is the loaded routing table: Rows[runner][tier] for the current row of each alias, ByModel[claudeID][runner]
// for every row, and each account key's billing type.
type Table struct {
	Billing map[string]string  `json:"billing"`
	Weights map[string]float64 `json:"weights"`
	// Rows is the tier view (opus/sonnet/haiku) of the rows the aliases currently point at.
	Rows map[string]map[string]Cell `json:"-"`
	// ByModel holds every row, keyed by canonical Claude id, then runner.
	ByModel map[string]map[string]Cell `json:"-"`
	// OpencodeSub are opencode's own sub-candidates, keyed by sub id (opencode-go, github-copilot, openai,
	// nvidia, zen-free) then tier; a missing tier (zen-free has no "opus") means that id never serves it.
	OpencodeSub map[string]map[string]Cell `json:"-"`
	// OpencodeLadder is the sub-pick's paid-backend order (set from the registry in LoadFrom, not JSON):
	// zen-free always runs first, then this order. Empty keeps whatever defaults.json/the vault overlay set.
	OpencodeLadder []string `json:"-"`
	// OpencodeRouting is the registry's routing.opencode object (set from the registry in LoadFrom, not
	// JSON): work-repo markers, which tiers zen-free may serve, and the free leg's stall deadline.
	OpencodeRouting providers.OpencodeRouting `json:"-"`
	// OpencodeRowOverridden marks a tier whose Rows["opencode"] cell a route overlay touched directly (set
	// from the registry in LoadFrom): that explicit operator intent wins over the sub-pick at that tier.
	OpencodeRowOverridden map[string]bool `json:"-"`
}

// fileTable is table.json's shape: rows keyed by canonical Claude id, then runner.
type fileTable struct {
	Billing     map[string]string          `json:"billing"`
	Weights     map[string]float64         `json:"weights"`
	Rows        map[string]map[string]Cell `json:"rows"`
	OpencodeSub map[string]map[string]Cell `json:"opencode_sub"`
}

// rowTier is the tier name of a Claude row id when an alias currently points at it, else "".
func rowTier(claudeID string) string {
	if a := models.AliasOf(claudeID); slices.Contains(Tiers, a) {
		return a
	}
	return ""
}

// rowKey resolves an overlay or lookup key (tier, alias, canonical id or any Claude spelling) to a row id.
func rowKey(key string) string {
	id, _, ok := models.Resolve(key)
	if !ok {
		return ""
	}
	return id
}

// CopilotUnpinned is the copilot account key when no gh user is pinned; route's account gate drops it.
const CopilotUnpinned = "github·unpinned"

// Load parses the embedded table, applies the registry's route overlay cell by cell and resolves account keys.
func Load() (*Table, error) {
	reg, _ := providers.Load() // a broken registry still routes on the embedded table
	return LoadFrom(reg, CopilotUser())
}

// LoadFrom loads the table against an explicit registry and Copilot gh user; Load() is the normal entry
// point, this is for tests and callers that already hold a registry.
func LoadFrom(reg *providers.Registry, ghUser string) (*Table, error) {
	var f fileTable
	if err := json.Unmarshal(tableJSON, &f); err != nil {
		return nil, fmt.Errorf("routetable: table.json: %w", err)
	}
	t := Table{Billing: f.Billing, Weights: f.Weights, ByModel: f.Rows, OpencodeRowOverridden: map[string]bool{}}
	copilotKey := CopilotUnpinned
	if ghUser != "" {
		copilotKey = "github·" + ghUser
	}
	t.Billing[copilotKey] = t.Billing["github·copilot"]
	if reg != nil {
		for runner, rows := range reg.Routing.Cells {
			for key, o := range rows {
				id, err := t.applyRouteOverlayCell(runner, key, o)
				if err != nil {
					return nil, err
				}
				if tier := rowTier(id); runner == "opencode" && tier != "" {
					t.OpencodeRowOverridden[tier] = true
				}
			}
		}
		if len(reg.Routing.Weights) > 0 {
			t.Weights = reg.Routing.Weights // an overlay replaces the weights whole, so they still sum to 1
		}
		t.OpencodeLadder = reg.Routing.Opencode.LadderOrder() // already normalized to sub ids by providers.Parse
		t.OpencodeRouting = reg.Routing.Opencode
	}
	if err := providers.ValidateRouteWeights(t.Weights); err != nil {
		return nil, fmt.Errorf("routetable: %w", err)
	}
	t.Rows = map[string]map[string]Cell{}
	for id, byRunner := range t.ByModel {
		c := byRunner["copilot"]
		c.Account = copilotKey
		byRunner["copilot"] = c
		if tier := rowTier(id); tier != "" {
			for runner, cell := range byRunner {
				if t.Rows[runner] == nil {
					t.Rows[runner] = map[string]Cell{}
				}
				t.Rows[runner][tier] = cell
			}
		}
	}
	t.OpencodeSub = map[string]map[string]Cell{}
	for sub, rows := range f.OpencodeSub {
		t.OpencodeSub[sub] = map[string]Cell{}
		for id, cell := range rows {
			if tier := rowTier(id); tier != "" {
				t.OpencodeSub[sub][tier] = cell
			}
		}
	}
	return &t, nil
}

// applyRouteOverlayCell merges one overlay cell (o) into ByModel[row][runner] and returns the row id. key is a
// canonical Claude id or alias.
// It resolves the opencode override account when the model prefix moves the cell off its table-defined backend.
func (t *Table) applyRouteOverlayCell(runner, key string, o providers.RouteCell) (string, error) {
	id := rowKey(key)
	c, ok := t.ByModel[id][runner]
	if !ok {
		return "", fmt.Errorf("routetable: overlay names unknown cell %s/%s", runner, key)
	}
	if o.Account == "" && o.Model != "" && runner == "opencode" && prefixOf(o.Model) != prefixOf(c.Model) {
		acct, err := opencodeOverrideAccount(prefixOf(o.Model))
		if err != nil {
			return "", fmt.Errorf("routetable: overlay %s/%s: %w", runner, key, err)
		}
		c.Account = acct
	}
	if o.Model != "" {
		c.Model = o.Model
	}
	if o.Effort != "" {
		c.Effort = o.Effort
	}
	if o.Account != "" {
		if _, ok := t.Billing[o.Account]; !ok {
			return "", fmt.Errorf("routetable: overlay %s/%s: account_key %q names no known billing entry", runner, key, o.Account)
		}
		c.Account = o.Account
	}
	t.ByModel[id][runner] = c
	return id, nil
}

// CopilotUser is the pinned copilot account name Copilot bills (github·<name>): the pinned github profile;
// the router never picks another account.
func CopilotUser() string {
	reg, _ := providers.Load()
	_, profile := providers.CopilotLogin(reg)
	return profile
}

// prefixOf returns the provider prefix of a "prefix/model" id ("" when model carries no prefix).
func prefixOf(model string) string {
	prefix, _, ok := strings.Cut(model, "/")
	if !ok {
		return ""
	}
	return prefix
}

// opencodeOverrideAccount derives the billing account for an opencode cell whose override model prefix moves
// it off the table's own opencode-go backend: opencode-go/opencode still bill opencode·go. github-copilot
// bills opencode·github-copilot, not the copilot row's pinned-gh-user key — opencode authenticates with its
// own stored GitHub Copilot login (vendors.OpencodeLogins), which may be a different GitHub user than the
// one Copilot's own row bills, so quota tracking must not conflate the two accounts. Any other prefix is
// rejected outright, since the cell would keep charging quota to opencode·go for a model that account never serves.
func opencodeOverrideAccount(prefix string) (string, error) {
	switch prefix {
	case "opencode-go", "opencode":
		return "opencode·go", nil
	case "github-copilot":
		return "opencode·github-copilot", nil
	case "openai":
		return "opencode·openai", nil
	case "nvidia":
		return "opencode·nvidia", nil
	default:
		return "", fmt.Errorf("model prefix %q has no known billing account; set account_key explicitly", prefix)
	}
}

// Row returns every runner's cell for a Claude row, named by alias (opus), short form (opus-5-5), canonical id or
// any wire spelling; ok is false when no row matches.
func (t *Table) Row(key string) (map[string]Cell, bool) {
	r, ok := t.ByModel[rowKey(key)]
	return r, ok
}

// CellFor returns the runner's cell of the Claude row named by key (alias or canonical id).
func (t *Table) CellFor(runner, key string) (Cell, bool) {
	c, ok := t.ByModel[rowKey(key)][runner]
	return c, ok
}

// Cell returns the runner's row at tier; ok is false for an unknown runner or tier.
func (t *Table) Cell(runner, tier string) (Cell, bool) {
	c, ok := t.Rows[runner][tier]
	return c, ok
}

// OpencodeSubCell returns opencode's sub-candidate id's row at tier; ok is false when id serves no row at
// that tier (zen-free has no "opus" row — that is how it stays out of the opus tier, not a tier check).
func (t *Table) OpencodeSubCell(id, tier string) (Cell, bool) {
	c, ok := t.OpencodeSub[id][tier]
	return c, ok
}

// TierOf maps a --model value to its tier: a slot name, a table cell's model, a Claude id's family, or a
// registry slot id; "" when nothing matches (the classifier decides then).
func (t *Table) TierOf(model string) string {
	model = models.Strip1M(strings.TrimSpace(model))
	if model == "" {
		return ""
	}
	if slices.Contains(Tiers, model) {
		return model
	}
	_, bare, _ := strings.Cut(model, ":") // provider:model
	if bare == "" {
		bare = model
	}
	for _, tier := range Tiers {
		for _, runner := range Runners {
			if m := t.Rows[runner][tier].Model; m == bare || m[strings.Index(m, "/")+1:] == bare {
				return tier
			}
		}
	}
	if f := models.FamilyOf(bare); slices.Contains(Tiers, f) {
		return f
	}
	if reg, err := providers.Load(); err == nil {
		for _, name := range reg.Names() {
			if s := reg.Providers[name].SlotOf(bare); slices.Contains(Tiers, s) {
				return s
			}
		}
	}
	return ""
}
