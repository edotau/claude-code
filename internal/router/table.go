// Package route picks the runner and model for one task: a runner × tier table (routetable, vault-overridable),
// each account's remembered quota state (quota.json), and a cost score. It reads state only and never dispatches.
package router

import (
	"github.com/edotau/claude-code/internal/router/routetable"
)

// Tiers are the routed tiers, strongest first; fable stays Claude Code's own slot.
var Tiers = routetable.Tiers

// Runners are the table's columns in roster order; ties in the cost score keep this order.
var Runners = routetable.Runners

// Billing types: a plan window already paid for, per-use credits, or a free third-party endpoint.
const (
	BillSubscription = routetable.BillSubscription
	BillCredits      = routetable.BillCredits
	BillFree         = routetable.BillFree
)

// Cell is one runner × tier row; an alias for routetable.Cell, the package both route and vendors load from.
type Cell = routetable.Cell

// Table is the loaded routing table, with route's own scoring (Pick) added over the routetable data it embeds.
type Table struct {
	routetable.Table
}

// Load parses the embedded table, applies the registry's route overlay and resolves the pinned Copilot account.
func Load() (*Table, error) {
	t, err := routetable.Load()
	if err != nil {
		return nil, err
	}
	return &Table{*t}, nil
}
