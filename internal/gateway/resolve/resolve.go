// Package resolve computes `claude-code gateway resolve`: first-hop routes, per-profile rows and the pool table.
package resolve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	dbxserving "github.com/edotau/claude-code/internal/databricks/serving"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/textutil"
)

// ProfileRow is one profile's outcome for --all-profiles: the route when it resolved, else why not.
type ProfileRow struct {
	Profile string            `json:"profile"`
	Pass    bool              `json:"pass"`
	Reason  string            `json:"reason,omitempty"`
	Route   *agent.ModelRoute `json:"route,omitempty"`
}

// googleQuotaCheck is the agy /usage seam; tests assert a non-Google route never calls it.
var googleQuotaCheck = gemini.Check

// WithGoogleQuota notes a Google route's pool percent and hold, warning on warnTo; a failed read never fails resolve.
func WithGoogleQuota(ctx context.Context, r agent.ModelRoute, warnTo io.Writer) agent.ModelRoute {
	if !router.PerModelQuota(r.Account) {
		return r
	}
	q, err := googleQuotaCheck(ctx, r.Model, time.Now())
	note := q.Note()
	switch {
	case err != nil:
		note = "usage unavailable"
	case q.HoldErr != nil:
		note += "; hold not saved"
	}
	r.Note = strings.Join(slices.DeleteFunc([]string{r.Note, note}, func(s string) bool { return s == "" }), "; ")
	if w := q.Warning(); w != "" {
		fmt.Fprintln(warnTo, w)
	}
	return r
}

// GoogleLabelWarning prints a gemini dispatch's low-quota warning to w; a route that cannot resolve prints nothing.
func GoogleLabelWarning(ctx context.Context, w io.Writer, name, model, profile, task string) {
	q, err := QueryForRoute(ctx, name, model, route.Route{Profile: profile}, "")
	if err != nil {
		return
	}
	reg, err := providers.Load()
	if err != nil {
		return
	}
	if r, _, err := FirstHop(ctx, reg, q, task); err == nil {
		WithGoogleQuota(ctx, r, w)
	}
}

// runQuery is a seam: tests stub it so resolve never reads the live route table.
var runQuery = agent.RunQuery

// FirstHop resolves q as the run's first hop does: an untyped external agent takes the ladder, like the label.
func FirstHop(ctx context.Context, reg *providers.Registry, q agent.ModelQuery, task string,
) (agent.ModelRoute, *agent.AutoChoice, error) {
	ctx = agent.PeekContext(ctx)
	hop, note, err := runQuery(ctx, reg, q, router.RuleClassifier{}.Alias(task))
	if err != nil {
		return agent.ModelRoute{}, nil, err
	}
	r, choice, err := agent.ExplainRun(reg, agent.QueryFor(ctx, hop), time.Now())
	r.Note = strings.Join(slices.DeleteFunc([]string{note, r.Note}, func(s string) bool { return s == "" }), "; ")
	return r, choice, err
}

// QueryForRoute resolves explicit --provider/--profile flags with --overlay; conflicting routes error.
func QueryForRoute(ctx context.Context, name, model string, flagRoute route.Route, overlay string) (agent.ModelQuery, error) {
	o, err := agentconfig.LoadOverlayContext(ctx, overlay, os.Stdin)
	if err != nil {
		return agent.ModelQuery{}, err
	}
	rt := o.Route()
	if !flagRoute.IsZero() {
		typed := flagRoute
		if typed.Profile != "" && typed.Profile != agent.AutoModel {
			reg, err := providers.Load()
			if err != nil {
				return agent.ModelQuery{}, err
			}
			if _, err := reg.Get(typed.Profile); err != nil {
				return agent.ModelQuery{}, err
			}
			typed.Provider = reg.Row(typed.Profile).Provider
		} else if typed.Provider == "" {
			typed.Provider = providers.GroupDatabricks
		}
		if !rt.IsZero() && rt != typed {
			return agent.ModelQuery{}, errors.New("--profile route conflicts with --overlay route")
		}
		rt = typed
	}
	return agent.QueryFor(ctx, agent.ModelQuery{Agent: name, Model: cmp.Or(model, o.Model), Route: rt, Profile: rt.Profile,
		Provider: rt.Provider, Account: rt.Account, Identity: rt.Identity, Effort: o.Effort, Fallback: o.Fallback}), nil
}

// All resolves q on every profile, optionally live-checking each that resolved.
func All(ctx context.Context, reg *providers.Registry, q agent.ModelQuery, live bool) []ProfileRow {
	q = agent.QueryFor(agent.PeekContext(ctx), q)
	target := route.Target{Model: q.Model}
	var rows []ProfileRow
	for _, p := range allRows(reg) {
		target.Route = route.Route{Provider: p.Provider, Profile: p.Name}
		q.Route, q.Model = target.Route, target.Model
		q.Profile, q.Provider, q.Account, q.Identity = target.Profile, target.Provider, target.Account, target.Identity
		row := ProfileRow{Profile: p.Name}
		r, err := agent.ResolveRun(reg, q, time.Now())
		if err != nil {
			row.Reason = strings.TrimPrefix(err.Error(), agent.ErrModelRoute.Error()+": ")
			rows = append(rows, row)
			continue
		}
		row.Route, row.Pass = &r, true
		if live {
			if why := liveCheck(ctx, p, r); why != "" {
				row.Pass, row.Reason = false, why
			}
		} else if r.Served == agent.ServedNo {
			row.Pass, row.Reason = false, r.Model+" is not listed"
		} // an unverified route says so on its own line: Line prints served: unverified
		rows = append(rows, row)
	}
	return rows
}

// allRows lists every registry row grouped by provider type, each in declared order.
func allRows(reg *providers.Registry) []*providers.Provider {
	var types []string
	for _, n := range reg.Names() {
		if t := reg.Providers[n].Provider; !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	var out []*providers.Provider
	for _, t := range types {
		for _, n := range reg.ProfilesOf(t) {
			out = append(out, reg.Providers[n])
		}
	}
	return out
}

// liveCheck is "" when p's credential mints headless, its listing is fresh and holds the model READY.
func liveCheck(ctx context.Context, p *providers.Provider, r agent.ModelRoute) string {
	ctx, cancel := context.WithTimeout(providers.WithIdentity(ctx, providers.IdentityHeadless), 45*time.Second)
	defer cancel()
	if _, err := providers.Credential(ctx, p); err != nil && !errors.Is(err, providers.ErrPassthrough) {
		return "credential: " + textutil.FirstLine(err.Error())
	}
	if p.Preset != providers.PresetDatabricks {
		return ""
	}
	now := time.Now()
	if msg := refreshServed(ctx, p, now); msg != "" {
		return msg
	}
	switch listed, ready, fresh := router.ServedListed(p, r.Model, now); {
	case !fresh:
		return "listing still stale after refresh"
	case !listed:
		return r.Model + " is not listed"
	case !ready:
		return r.Model + " endpoint is not READY"
	}
	return ""
}

// refreshServed is "" when p's served listing is fresh, or was refreshed and saved.
func refreshServed(ctx context.Context, p *providers.Provider, now time.Time) string {
	if s, ok := serving.Load(p.Name); ok && s.Fresh(now) {
		return ""
	}
	infos, err := providers.ListModels(ctx, p)
	if err != nil {
		return "listing refresh: " + textutil.FirstLine(err.Error())
	}
	if err := dbxserving.WarmServed(p.Name, infos, now); err != nil {
		return "listing save: " + err.Error()
	}
	return ""
}
