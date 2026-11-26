package ask

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	usagepkg "github.com/edotau/claude-code/internal/dev/usage"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/router"
)

// RouteFlags holds the ask flags the wave runner shares: --overlay and --fallback.
type RouteFlags struct{ f askFlags }

// RegisterRouteFlags adds those flags to fs through ask's own definitions.
func RegisterRouteFlags(fs *flag.FlagSet) *RouteFlags {
	r := &RouteFlags{}
	registerFallbackFlag(fs, &r.f)
	registerOverlayFlag(fs, &r.f)
	return r
}

// Args re-emits the route flags as a single --overlay document, so a child ask run routes as the caller did.
func (r *RouteFlags) Args() ([]string, error) {
	f := &r.f
	if f.overlay == "-" {
		return nil, errors.New("--overlay - reads stdin, which a batch child needs; pass a file or inline JSON")
	}
	if f.fallback == "" {
		if f.overlay == "" {
			return nil, nil
		}
		return []string{"--overlay", f.overlay}, nil
	}
	var base agentconfig.Overlay
	if f.overlay != "" {
		var err error
		if base, err = agentconfig.LoadOverlayFrom(context.Background(), f.overlay, nil, true); err != nil {
			return nil, err
		}
	}
	ov, err := base.Derive(agentconfig.Overlay{Fallback: f.fallback}, agentconfig.HopAppend)
	if err != nil {
		return nil, err
	}
	return []string{"--overlay", ov.Doc()}, nil
}

// failSpawn finishes a record whose spawn failed so it never shows as running; a failed save joins the returned error.
func failSpawn(rec *agent.Record, err error) error {
	cur, _, uerr := agent.UpdateRunning(rec.ID, func(r agent.Record) agent.Record { // only a still-running record
		r.Finish(wire.Result{}, err, time.Since(r.Time))
		return r
	})
	if errors.Is(uerr, os.ErrNotExist) { // never saved: write it settled
		rec.Finish(wire.Result{}, err, time.Since(rec.Time))
		uerr = rec.Save()
	} else if uerr == nil {
		*rec = cur
	}
	rec.NoteHealth(err)
	if uerr != nil {
		err = errors.Join(err, fmt.Errorf("save failed dispatch %s: %w", rec.ID, uerr))
	}
	return err
}

// RouteRefresh re-fetches a stale account for a pick; a var so tests stay offline.
var RouteRefresh = usagepkg.RefreshAccount

// newClient builds the dispatch client; RouteRefresh is the offline-able account refresh.
func newClient() *agent.Client {
	return agent.NewClient(router.RuleClassifier{}.Alias, RouteRefresh,
		func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) })
}
