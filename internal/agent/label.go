package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// RunQuery is q as the run's first hop resolves it; a routed external dispatch takes the ladder's first hop.
func RunQuery(ctx context.Context, reg *providers.Registry, q ModelQuery, aliasHint string) (ModelQuery, string, error) {
	if q.Agent == agentid.Claude || q.Model != "" || q.Profile != "" || q.Provider != "" || q.Account != "" {
		return q, "", nil
	}
	if QueryFor(ctx, q).PinModel == router.DiscoveryModel { // the ladder would advance the rotation; a label must not
		return q, "a discovery pin picks its model at run time", nil
	}
	chain, _, err := Resolve(ctx, RouteReq{Agent: q.Agent, AliasHint: aliasHint, Fallback: q.Fallback, Pinned: true})
	if err != nil {
		return q, "", err
	}
	if len(chain.Dispatches) == 0 {
		return q, "", fmt.Errorf("no route runs %s (%s)", q.Agent, strings.Join(chain.Skipped, "; "))
	}
	return ladderHop(reg, q, chain.Dispatches[0])
}

// ladderHop is the query d's engine resolves: a `<row>:<model>` cell as typed, an alias cell the row's pin or opus.
func ladderHop(reg *providers.Registry, q ModelQuery, d Dispatch) (ModelQuery, string, error) {
	hop := q
	hop.Agent = d.Agent.Name()
	acct := d.Account
	if _, row, ok := strings.Cut(acct, "·"); ok {
		acct = row
	}
	if reg.Row(acct) != nil {
		hop.Profile = acct
	}
	if !d.keepCaller && !d.unroutedDispatch && (strings.Contains(d.Model, ":") || hop.Agent == agentid.Claude) {
		hop.Model = d.Model // dispatchRequest's CellModel: a bare alias is dropped for a vendor CLI's own pin
	}
	note := ""
	if hop.Agent != q.Agent {
		note = fmt.Sprintf("%s is gated out; the route runs %s", q.Agent, hop.Agent)
	}
	return hop, note, nil
}
