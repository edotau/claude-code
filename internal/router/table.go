package router

import (
	"cmp"
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

// Aliases are the routed aliases, strongest first; fable stays Claude Code's own alias.
var Aliases = providers.RouteAliases

// TableAgents are the table's columns in roster order; ties in a pick's cost score keep this order.
var TableAgents = providers.RouteAgents

// OpencodeTableModels reads the opencode sonnet/haiku cells for the opencode overlay; ok is false on a load error.
func OpencodeTableModels() (string, string, bool) {
	t, err := Load()
	if err != nil {
		return "", "", false
	}
	sonnet, ok := t.Cell("opencode", "sonnet")
	if !ok || sonnet.Model == "" {
		return "", "", false
	}
	haiku, ok := t.Cell("opencode", "haiku")
	if !ok || haiku.Model == "" {
		return "", "", false
	}
	return sonnet.Model, haiku.Model, true
}

// Price is USD per 1M tokens.
type Price struct {
	In       float64 `json:"in"`
	CachedIn float64 `json:"cached_in"`
	Out      float64 `json:"out"`
}

// Cell is one agent × alias row.
type Cell struct {
	Model   string `json:"model"`
	Effort  string `json:"effort,omitempty"`
	Account string `json:"account_key"`
	Price   Price  `json:"price"`
}

// RouteTable is the loaded routing table: Rows[agent][alias] for the current row of each alias, ByModel[claudeID][agent]
// for every row, and each account key's billing type.
type RouteTable struct {
	Billing map[string]string  `json:"billing"`
	Weights map[string]float64 `json:"weights"`
	// Rows is the alias view (opus/sonnet/haiku) of the rows the aliases currently point at.
	Rows map[string]map[string]Cell `json:"-"`
	// ByModel holds every row, keyed by canonical Claude id, then agent.
	ByModel map[string]map[string]Cell `json:"-"`
	// OpencodeSub are opencode's own sub-candidates, keyed by sub id (opencode-go, github-copilot, openai,
	// nvidia, zen-free) then alias; a missing alias (zen-free has no "opus") means that id never serves it.
	OpencodeSub map[string]map[string]Cell `json:"-"`
	// OpencodeLadder is the sub-pick's paid-backend order (set from the registry in LoadFrom, not JSON):
	// zen-free always runs first, then this order. Empty keeps whatever defaults.json/the vault overlay set.
	OpencodeLadder []string `json:"-"`
	// OpencodeRouting is the registry's routing.opencode object (set from the registry in LoadFrom, not
	// JSON): work-repo markers, which aliases zen-free may serve, and the free dispatch's stall deadline.
	OpencodeRouting providers.OpencodeRouting `json:"-"`
	// OpencodeRowOverridden marks an alias whose Rows["opencode"] cell a route overlay touched directly (set
	// from the registry in LoadFrom): that explicit operator intent wins over the sub-pick at that alias.
	OpencodeRowOverridden map[string]bool `json:"-"`
	// Ladders are the registry's routing.ladders (set in LoadFrom): the fallback order after want, by ladder name.
	Ladders map[string][]providers.Rung `json:"-"`
	// ClaudeAnywhere is routing.claudeAnywhere: claude scores into the ladder instead of going last.
	ClaudeAnywhere bool `json:"-"`

	reg *providers.Registry // the registry LoadFrom applied; AliasOf reads its provider aliases
}

// fileTable is table.json's shape: rows keyed by canonical Claude id, then agent.
type fileTable struct {
	Billing     map[string]string          `json:"billing"`
	Weights     map[string]float64         `json:"weights"`
	Rows        map[string]map[string]Cell `json:"rows"`
	OpencodeSub map[string]map[string]Cell `json:"opencode_sub"`
}

// rowAlias is the alias name of a Claude row id when an alias currently points at it, else "".
func rowAlias(claudeID string) string {
	if a := models.AliasOf(claudeID); slices.Contains(Aliases, a) {
		return a
	}
	return ""
}

// rowKey resolves an overlay or lookup key (alias, canonical id or any Claude spelling) to a row id.
func rowKey(key string) string {
	id, _, ok := models.Resolve(key)
	if !ok {
		return ""
	}
	return id
}

// copilotUnpinned is the copilot account key when no gh user is pinned; route's account gate drops it.
const copilotUnpinned = "github·unpinned"

// Load parses the embedded table, applies the registry's route overlay cell by cell and resolves account keys.
func Load() (*RouteTable, error) {
	reg, _ := providers.Load() // a broken registry still routes on the embedded table
	return LoadFrom(reg, copilotUserFrom(reg))
}

// LoadFrom loads the table against an explicit registry and Copilot gh user; Load() is the normal entry
// point, this is for tests and callers that already hold a registry.
func LoadFrom(reg *providers.Registry, ghUser string) (*RouteTable, error) {
	var f fileTable
	if err := json.Unmarshal(tableJSON, &f); err != nil {
		return nil, fmt.Errorf("router: table.json: %w", err)
	}
	t := RouteTable{Billing: f.Billing, Weights: f.Weights, ByModel: f.Rows, OpencodeRowOverridden: map[string]bool{}}
	copilotKey := copilotUnpinned
	if ghUser != "" {
		copilotKey = "github·" + ghUser
	}
	t.Billing[copilotKey] = t.Billing["github·copilot"]
	t.reg = reg
	if err := t.applyRegistry(reg); err != nil {
		return nil, err
	}
	if err := providers.ValidateRouteWeights(t.Weights); err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	t.indexRows(copilotKey)
	t.OpencodeSub = opencodeSubAliases(f.OpencodeSub)
	return &t, nil
}

// applyRegistry applies the registry's route overlay, weights and opencode ladder; a nil registry changes nothing.
func (t *RouteTable) applyRegistry(reg *providers.Registry) error {
	if reg == nil {
		return nil
	}
	for agent, rows := range reg.Routing.Cells {
		for key, o := range rows {
			id, err := t.applyRouteOverlayCell(agent, key, o)
			if err != nil {
				return err
			}
			if alias := rowAlias(id); agent == "opencode" && alias != "" {
				t.OpencodeRowOverridden[alias] = true
			}
		}
	}
	if len(reg.Routing.Weights) > 0 {
		t.Weights = reg.Routing.Weights // an overlay replaces the weights whole, so they still sum to 1
	}
	t.OpencodeLadder = reg.Routing.Opencode.LadderOrder() // already normalized to sub ids by providers.Parse
	t.OpencodeRouting = reg.Routing.Opencode
	t.Ladders, t.ClaudeAnywhere = reg.Routing.Ladders, reg.Routing.ClaudeAnywhereOn()
	return nil
}

// indexRows stamps copilot's account key on every row and builds the per-alias Rows view.
func (t *RouteTable) indexRows(copilotKey string) {
	t.Rows = map[string]map[string]Cell{}
	for id, byAgent := range t.ByModel {
		c := byAgent["copilot"]
		c.Account = copilotKey
		byAgent["copilot"] = c
		alias := rowAlias(id)
		if alias == "" {
			continue
		}
		for agent, cell := range byAgent {
			if t.Rows[agent] == nil {
				t.Rows[agent] = map[string]Cell{}
			}
			t.Rows[agent][alias] = cell
		}
	}
}

// opencodeSubAliases re-keys each opencode sub-candidate's rows by alias.
func opencodeSubAliases(subs map[string]map[string]Cell) map[string]map[string]Cell {
	out := map[string]map[string]Cell{}
	for sub, rows := range subs {
		out[sub] = map[string]Cell{}
		for id, cell := range rows {
			if alias := rowAlias(id); alias != "" {
				out[sub][alias] = cell
			}
		}
	}
	return out
}

// applyRouteOverlayCell merges one overlay cell (o) into ByModel[row][agent] and returns the row id.
func (t *RouteTable) applyRouteOverlayCell(agent, key string, o providers.RouteCell) (string, error) {
	id := rowKey(key)
	c, ok := t.ByModel[id][agent]
	if !ok {
		return "", fmt.Errorf("router: overlay names unknown cell %s/%s", agent, key)
	}
	if o.Account == "" && o.Model != "" && agent == "opencode" && prefixOf(o.Model) != prefixOf(c.Model) {
		acct, err := opencodeOverrideAccount(prefixOf(o.Model))
		if err != nil {
			return "", fmt.Errorf("router: overlay %s/%s: %w", agent, key, err)
		}
		c.Account = acct
	}
	c.Model = cmp.Or(o.Model, c.Model)
	c.Effort = cmp.Or(o.Effort, c.Effort)
	if o.Account != "" {
		if _, ok := t.Billing[o.Account]; !ok {
			return "", fmt.Errorf("router: overlay %s/%s: account_key %q names no known billing entry", agent, key, o.Account)
		}
		c.Account = o.Account
	}
	t.ByModel[id][agent] = c
	return id, nil
}

// CopilotUser is the pinned copilot account name Copilot bills (github·<name>): the pinned github profile;
// the router never picks another account.
func CopilotUser() string {
	reg, _ := providers.Load()
	return copilotUserFrom(reg)
}

func copilotUserFrom(reg *providers.Registry) string {
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

// opencodeOverrideAccount is the billing account for an opencode cell whose model prefix leaves opencode-go.
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

// Cell returns the agent's row at alias; ok is false for an unknown agent or alias.
func (t *RouteTable) Cell(agent, alias string) (Cell, bool) {
	c, ok := t.Rows[agent][alias]
	return c, ok
}

// AliasOf maps a --model value to its alias: an alias name, a table cell's model, a Claude id's family, or a
// registry alias id; "" when nothing matches (the classifier decides then).
func (t *RouteTable) AliasOf(model string) string {
	model = models.Strip1M(strings.TrimSpace(model))
	if model == "" {
		return ""
	}
	if slices.Contains(Aliases, model) {
		return model
	}
	ids := []string{model}
	if _, bare, _ := strings.Cut(model, ":"); bare != "" {
		ids = append(ids, bare)
	}
	for _, id := range ids {
		if alias := t.aliasOfCell(id); alias != "" {
			return alias
		}
		if f := models.FamilyOf(id); slices.Contains(Aliases, f) {
			return f
		}
		if alias := t.registryAliasOf(id); alias != "" {
			return alias
		}
	}
	return ""
}

// aliasOfCell is the alias whose table cell (any agent) runs bare, with or without a provider prefix; "" if none.
func (t *RouteTable) aliasOfCell(bare string) string {
	for _, alias := range Aliases {
		for _, agent := range TableAgents {
			if m := t.Rows[agent][alias].Model; m == bare || m[strings.Index(m, "/")+1:] == bare {
				return alias
			}
		}
	}
	return ""
}

// registryAliasOf is the alias some provider of the loaded registry maps bare to; "" if none or no registry.
func (t *RouteTable) registryAliasOf(bare string) string {
	if t.reg == nil {
		return ""
	}
	for _, name := range t.reg.Names() {
		if s := t.reg.Providers[name].AliasOf(bare); slices.Contains(Aliases, s) {
			return s
		}
	}
	return ""
}
