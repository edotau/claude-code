package router

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

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

// parsedTable is table.json decoded once per process; LoadFrom clones what it mutates.
var parsedTable = sync.OnceValues(func() (fileTable, error) {
	var f fileTable
	err := json.Unmarshal(tableJSON, &f)
	return f, err
})

// databricksTiers is table.json's "databricks" table: each alias's ordered bucket of canonical ids, shared by agents.
func databricksTiers() map[string][]string {
	f, _ := parsedTable() // a malformed table.json fails LoadFrom, which every pick runs first
	return f.Databricks
}

// DatabricksModel is the first member of alias's bucket agent runs and nothing marks unusable, for p; "" if none.
func DatabricksModel(agent, alias string, p *providers.Provider) string {
	if p == nil || p.Preset != providers.PresetDatabricks {
		return ""
	}
	now, st := time.Now(), LoadState()
	dead := deadAccounts(ProviderAccount(p.Name)) // dead[0] is p's own account, where MarkUnserved writes
	for _, id := range databricksTiers()[alias] {
		wire := SpellServed(p, id, now)
		if _, _, known := models.Resolve(id); !known && wire == id {
			continue // an unlisted id with no READY endpoint to spell it by
		}
		if bucketRunnable(agent, p, wire) && !tierUnusable(st, dead, p, wire, now) {
			return wire
		}
	}
	return ""
}

// bucketRunnable reports whether agent can run wire: claude and gemini their own family, codex a Responses model.
func bucketRunnable(agent string, p *providers.Provider, wire string) bool {
	switch agent {
	case "claude":
		return models.FamilyKind(wire) == models.FamilyClaude
	case "gemini":
		return models.FamilyKind(wire) == models.FamilyGemini
	case "codex":
		return providers.ServesResponses(p, wire) // the Responses wire codex speaks (providers.WireFor)
	}
	return true
}

// tierUnusable reports wire missing or not READY in p's fresh listing, marked unserved, or quota-dead on dead.
func tierUnusable(st State, dead []string, p *providers.Provider, wire string, now time.Time) bool {
	if _, ready, fresh := ServedListed(p, wire, now); fresh && !ready {
		return true // ready is false for an unlisted endpoint too
	}
	return unservedMarked(st[dead[0]], p, wire, now) || st.modelDeadOn(dead, wire, now)
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

// RouteTable holds alias rows, canonical model rows, and account billing types.
type RouteTable struct {
	Billing map[string]string  `json:"billing"`
	Weights map[string]float64 `json:"weights"`
	// Rows is the alias view (opus/sonnet/haiku) of the rows the aliases currently point at.
	Rows map[string]map[string]Cell `json:"-"`
	// ByModel holds every row, keyed by canonical Claude id, then agent.
	ByModel map[string]map[string]Cell `json:"-"`
	// OpencodeSub maps sub id then alias; a missing alias (e.g. zen-free opus) is never served.
	OpencodeSub map[string]map[string]Cell `json:"-"`
	// OpencodeLadder is paid order from LoadFrom; zen-free runs first; empty keeps registry defaults/overlay.
	OpencodeLadder []string `json:"-"`
	// OpencodeRouting holds registry work markers, free aliases and stall deadlines, populated by LoadFrom.
	OpencodeRouting providers.OpencodeRouting `json:"-"`
	// OpencodeRowOverridden marks explicit overlay cells that override the sub-pick, populated by LoadFrom.
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
	Databricks  map[string][]string        `json:"databricks"`
}

// cloneRows copies rows two levels deep, so a table's overlay never writes the shared parse.
func cloneRows(rows map[string]map[string]Cell) map[string]map[string]Cell {
	out := make(map[string]map[string]Cell, len(rows))
	for id, byAgent := range rows {
		out[id] = maps.Clone(byAgent)
	}
	return out
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

// LoadFrom loads against an explicit registry and Copilot user; Load is the normal entry point.
func LoadFrom(reg *providers.Registry, ghUser string) (*RouteTable, error) {
	f, err := parsedTable()
	if err != nil {
		return nil, fmt.Errorf("router: table.json: %w", err)
	}
	t := RouteTable{Billing: maps.Clone(f.Billing), Weights: maps.Clone(f.Weights), ByModel: cloneRows(f.Rows),
		OpencodeRowOverridden: map[string]bool{}}
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
	account, err := overlayCellAccount(agent, key, o, c)
	if err != nil {
		return "", err
	}
	c.Account = account
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

// CopilotUser is the pinned github profile billed as github·<name>; the router never picks another account.
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

// AliasOf maps aliases, table models, Claude families or registry alias ids; unmatched returns "" for classification.
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

func overlayCellAccount(agent, key string, o providers.RouteCell, c Cell) (string, error) {
	if o.Account == "" && o.Model != "" && agent == "opencode" && prefixOf(o.Model) != prefixOf(c.Model) {
		acct, err := opencodeOverrideAccount(prefixOf(o.Model))
		if err != nil {
			return "", fmt.Errorf("router: overlay %s/%s: %w", agent, key, err)
		}
		c.Account = acct
	}
	return c.Account, nil
}

// ModelRow resolves a --model value to its row's alias and canonical Claude id; exact is a non-Claude id as asked.
func ModelRow(model string) (alias, row, exact string) {
	if model == DiscoveryModel {
		return "", "", ""
	}
	canonical, _, ok := models.Resolve(model)
	switch {
	case !ok && strings.TrimSpace(model) != "": // not in the catalog: still an exact request, never a silent default
		return "", "", strings.TrimSpace(model)
	case !ok:
		return "", "", ""
	case models.FamilyKind(canonical) != models.FamilyClaude:
		return "", "", canonical
	}
	alias = cmp.Or(models.AliasOf(canonical), models.FamilyOf(canonical))
	if !slices.Contains(Aliases, alias) {
		alias = ""
	}
	return alias, canonical, ""
}

// ClaudeCellModel is the alias a canonical claude cell id backs, so session pins resolve it; else the id itself.
func ClaudeCellModel(cell string) string {
	canonical, _, ok := models.Resolve(cell)
	if !ok {
		return cell
	}
	return cmp.Or(models.AliasOf(canonical), canonical)
}

// LowestAliasServing is the lowest alias (haiku up) whose cell for agent resolves to id, else "".
func (t *RouteTable) LowestAliasServing(agent, id string) string {
	for _, alias := range slices.Backward(Aliases) {
		if c, ok := t.Rows[agent][alias]; ok {
			if cid, _, ok := models.Resolve(c.Model); ok && cid == id {
				return alias
			}
		}
	}
	return ""
}

// CellServing is agent's cell (any row, then the opencode sub-candidates when hub) whose model is exact.
func (t *RouteTable) CellServing(agent, exact string, hub bool) (Cell, bool) {
	serves := func(c Cell) bool { id, _, ok := models.Resolve(c.Model); return ok && id == exact }
	for _, row := range slices.Sorted(maps.Keys(t.ByModel)) {
		if c, ok := t.ByModel[row][agent]; ok && serves(c) {
			return c, true
		}
	}
	if !hub {
		return Cell{}, false
	}
	for _, sub := range slices.Sorted(maps.Keys(t.OpencodeSub)) {
		for _, alias := range slices.Sorted(maps.Keys(t.OpencodeSub[sub])) {
			if c := t.OpencodeSub[sub][alias]; serves(c) {
				return c, true
			}
		}
	}
	return Cell{}, false
}

// PreviewAlias is the alias a preview routes model at: its own, else the task's classified one, else sonnet.
func (t *RouteTable) PreviewAlias(model, task string) string {
	hint := t.AliasOf(model)
	if hint == "" && task != "" {
		hint = RuleClassifier{}.Alias(task)
	}
	return cmp.Or(hint, Aliases[1])
}
