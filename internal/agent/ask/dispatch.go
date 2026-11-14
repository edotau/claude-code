package ask

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/router"
	usagepkg "github.com/edotau/claude-code/internal/usage"
)

// RouteFlags holds the ask flags the wave runner shares: --overlay, --fallback and the permission flags.
type RouteFlags struct{ f askFlags }

// RegisterRouteFlags adds those flags to fs through ask's own definitions.
func RegisterRouteFlags(fs *flag.FlagSet) *RouteFlags {
	r := &RouteFlags{}
	registerFallbackFlag(fs, &r.f)
	registerPermFlags(fs, &r.f)
	registerOverlayFlag(fs, &r.f)
	return r
}

// Args re-emits the route flags that were set as `ask` arguments, so a child ask run routes as the caller did.
func (r *RouteFlags) Args() ([]string, error) {
	f := &r.f
	if f.overlay == "-" {
		return nil, errors.New("--overlay - reads stdin, which a batch child needs; pass a file or inline JSON")
	}
	var out []string
	for _, kv := range [][2]string{{"overlay", f.overlay}, {"fallback", f.fallback}, {"mode", f.permMode}} {
		if kv[1] != "" {
			out = append(out, "--"+kv[0], kv[1])
		}
	}
	for _, list := range []struct {
		name  string
		rules []string
	}{{"allow", f.permAllow}, {"ask", f.permAsk}, {"deny", f.permDeny}} {
		for _, rule := range list.rules {
			out = append(out, "--"+list.name, rule)
		}
	}
	return out, nil
}

// failSpawn finishes a record whose spawn failed so it never shows as running; a failed save joins the returned error.
func failSpawn(rec *agent.Record, err error) error {
	rec.Finish(wire.Result{}, err, time.Since(rec.Time))
	if saveErr := rec.Save(); saveErr != nil {
		err = errors.Join(err, fmt.Errorf("save failed dispatch %s: %w", rec.ID, saveErr))
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
