// Package routetable is the embedded runner × tier routing grid (table.json, vault-overridable): the Table,
// Cell and Price types and Load. It sits below internal/route (which scores and picks a runner) and
// internal/vendors (which renders the opencode overlay from a cell's model), so neither of those two needs to
// import the other through this data.
package routetable

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

//go:embed table.json
var tableJSON []byte

// Tiers are the routed tiers, strongest first; fable stays Claude Code's own slot.
var Tiers = []string{models.Opus, models.Sonnet, models.Haiku}

// Runners are the table's columns in roster order; ties in a pick's cost score keep this order.
var Runners = []string{"claude", "copilot", "codex", "opencode", "gemini"}

// Billing types: a plan window already paid for, or per-use credits.
const (
	BillSubscription = "subscription"
	BillCredits      = "credits"
)

// weightNames are the dimension names table.json's weights must sum to 1 over; they must match the Name()
// of every internal/route.Dimension (today: "price", "headroom") — route owns the scoring, this package only
// validates the shape of what it loads.
var weightNames = []string{"price", "headroom"}

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

// Table is the loaded routing table: Rows[runner][tier] and each account key's billing type.
type Table struct {
	Billing map[string]string          `json:"billing"`
	Weights map[string]float64         `json:"weights"`
	Rows    map[string]map[string]Cell `json:"rows"`
}

// CopilotUnpinned is the copilot account key when no gh user is pinned; route's account gate drops it.
const CopilotUnpinned = "copilot:unpinned"

// Load parses the embedded table, applies the registry's route overlay cell by cell and resolves account keys.
func Load() (*Table, error) {
	reg, _ := providers.Load() // a broken registry still routes on the embedded table
	return LoadFrom(reg, CopilotUser())
}

// LoadFrom loads the table against an explicit registry and Copilot gh user; Load() is the normal entry
// point, this is for tests and callers that already hold a registry.
func LoadFrom(reg *providers.Registry, ghUser string) (*Table, error) {
	var t Table
	if err := json.Unmarshal(tableJSON, &t); err != nil {
		return nil, fmt.Errorf("routetable: table.json: %w", err)
	}
	if reg != nil {
		for runner, tiers := range reg.Route {
			for tier, o := range tiers {
				if _, ok := t.Rows[runner][tier]; !ok {
					return nil, fmt.Errorf("routetable: overlay names unknown cell %s/%s", runner, tier)
				}
				c := t.Rows[runner][tier]
				if o.Model != "" {
					c.Model = o.Model
				}
				if o.Effort != "" {
					c.Effort = o.Effort
				}
				t.Rows[runner][tier] = c
			}
		}
	}
	if reg != nil && len(reg.RouteWeights) > 0 {
		t.Weights = reg.RouteWeights // an overlay replaces the weights whole, so they still sum to 1
	}
	if err := t.validateWeights(); err != nil {
		return nil, err
	}
	copilotKey := CopilotUnpinned
	if ghUser != "" {
		copilotKey = "copilot:" + ghUser
	}
	t.Billing[copilotKey] = t.Billing["copilot"]
	for _, tier := range Tiers {
		c := t.Rows["copilot"][tier]
		c.Account = copilotKey
		t.Rows["copilot"][tier] = c
	}
	return &t, nil
}

// CopilotUser is the pinned gh user Copilot bills (COPILOT_GITHUB_USER); the router never picks another account.
func CopilotUser() string {
	if u := providers.Pin(providers.PinGitHubUser); u != "" {
		return u
	}
	return ""
}

// validateWeights requires one weight per dimension name, each in [0,1], summing to 1.
func (t *Table) validateWeights() error {
	sum := 0.0
	for _, name := range weightNames {
		w, ok := t.Weights[name]
		if !ok || w < 0 || w > 1 {
			return fmt.Errorf("routetable: weight %q missing or outside [0,1]", name)
		}
		sum += w
	}
	if len(t.Weights) != len(weightNames) {
		return fmt.Errorf("routetable: weights name %d dimensions, want %d", len(t.Weights), len(weightNames))
	}
	if math.Abs(sum-1) > 1e-9 {
		return fmt.Errorf("routetable: weights sum to %g, want 1", sum)
	}
	return nil
}

// Cell returns the runner's row at tier; ok is false for an unknown runner or tier.
func (t *Table) Cell(runner, tier string) (Cell, bool) {
	c, ok := t.Rows[runner][tier]
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
