package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxy/proxyteam"
	"github.com/edotau/claude-code/internal/routing/providers"
	"github.com/edotau/claude-code/internal/routing/route"
)

type (
	proxyGroupKey  struct{}
	scopedProxyKey struct{}
)

// WithProxyGroup marks ctx with the group id whose phase proxy the dispatch should use.
func WithProxyGroup(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, proxyGroupKey{}, id)
}

// WithScopedProxy makes the launcher walk the team route list for this ctx; agent names it in the route note.
func WithScopedProxy(ctx context.Context, agent string) context.Context {
	return context.WithValue(ctx, scopedProxyKey{}, agent)
}

// routeCand is one team on the route list with its scope (Port 0 when none is known).
type routeCand struct {
	team  proxyteam.Team
	scope proxy.Scope
}

// DispatchFacts derives the team-selection facts from a dispatch record.
func DispatchFacts(rec Record) proxyteam.Facts {
	carried := rec.Request.Carry
	f := proxyteam.Facts{
		Group:   rec.BatchID != "" || rec.PlanTask != "",
		Carried: carried,
		Nested:  rec.Parent != "" && !carried,
		Memory:  slices.Contains(rec.Request.EnvNames, memory.UpdateChildEnv),
	}
	if rec.Tree != "" {
		if st, err := os.Stat(filepath.Join(rec.Tree, ".git")); err == nil && st.Mode().IsRegular() {
			f.Lane = true
		}
	}
	return f
}

// inheritedScope is the team and id of the unit's endpoint when it names a scope proxy, else ok false.
func inheritedScope(ctx context.Context) (team, id string, ok bool) {
	u, err := RunUnit(ctx)
	e := u.Endpoint
	if err != nil || !isTeamLabel(e) { // a bad overlay already failed the ask; none inherits
		return "", "", false
	}
	team, id, _ = strings.Cut(e, ":")
	return team, id, true
}

// proxyGroupID is the ctx group id, else the id part of the unit's endpoint when its team part is team.
func proxyGroupID(ctx context.Context, team string) string {
	if g, _ := ctx.Value(proxyGroupKey{}).(string); g != "" {
		return g
	}
	if t, id, ok := inheritedScope(ctx); ok && t == team {
		return id
	}
	return ""
}

// InheritedProxyLive reports whether the unit's endpoint names a live scope proxy.
func InheritedProxyLive(ctx context.Context) bool {
	team, id, ok := inheritedScope(ctx)
	if !ok {
		return false
	}
	if reg, err := providers.Load(); err == nil {
		if u, _ := RunUnit(ctx); u.Profile != "" && reg.Row(u.Profile) != nil && reg.Row(u.Profile).Auth.Method() == providers.AuthNone {
			return false // a keyless row resolves direct, so its team label sits behind no proxy
		}
	}
	sc, err := proxy.ReadScope(team, id)
	return err == nil && scopeLive(ctx, sc) == ""
}

// parentTeamFirst puts the parent's phase team first per the plan; the fence is this process's own, so nothing widens.
func parentTeamFirst(ctx context.Context, cfg proxyteam.Config, routes []proxyteam.Team) []proxyteam.Team {
	name, _, ok := inheritedScope(ctx)
	i := slices.IndexFunc(cfg.Teams, func(t proxyteam.Team) bool { return t.Name == name && t.Host == proxyteam.HostPhase })
	if !ok || i < 0 || routes[0].Name == name {
		return routes
	}
	rest := slices.DeleteFunc(slices.Clone(routes), func(t proxyteam.Team) bool { return t.Name == name })
	return append([]proxyteam.Team{cfg.Teams[i]}, rest...)
}

// routeCands is the ordered team route list for this dispatch, each with its scope.
func routeCands(ctx context.Context) []routeCand {
	var rec Record
	if id, _ := ctx.Value(dispatchIDKey{}).(string); id != "" {
		rec, _ = LoadRecord(id)
	}
	facts := DispatchFacts(rec)
	facts.Interactive = rec.ID == "" // no dispatch record: a session, not a dispatch
	if g, _ := ctx.Value(proxyGroupKey{}).(string); g != "" {
		facts.Group = true
	}
	cfg, _ := proxyteam.Load(proxy.TeamOverlay())
	var out []routeCand
	routes := cfg.Routes(proxyteam.KindOf(facts))
	if rec.ID != "" {
		routes = parentTeamFirst(ctx, cfg, routes)
	}
	for _, t := range routes {
		sc := proxy.Scope{Team: t.Name}
		if t.Host == proxyteam.HostPhase {
			sc.ID = proxyGroupID(ctx, t.Name)
			if got, err := proxy.ReadScope(t.Name, sc.ID); err == nil {
				sc = got
			}
		}
		out = append(out, routeCand{team: t, scope: sc})
	}
	return out
}

func (c routeCand) label() string {
	if c.scope.ID != "" {
		return c.team.Name + ":" + c.scope.ID
	}
	return c.team.Name
}

// scopeLive is "" when the scope's process is alive and healthy, else the short reason it is not.
func scopeLive(ctx context.Context, sc proxy.Scope) string {
	if sc.Port <= 0 {
		return "no scope file"
	}
	start, err := proc.StartTime(sc.PID)
	if errors.Is(err, proc.ErrNoProcess) || (err == nil && start != sc.Start) {
		return "process gone"
	}
	st, ok := proxy.Holder(ctx, sc.Port)
	switch {
	case !ok:
		return "health: no healthy proxy answers"
	case st.PID != 0 && st.PID != sc.PID: // another process took the port
		return "port held by another process"
	}
	return ""
}

// ensureTeam is a seam for tests; it finds or starts a detached team's proxy.
var ensureTeam = proxy.EnsureTeam

// teamFailed reports whether any slot of team died mid-run; a dispatch maps to one slot, so this is its slot.
func teamFailed(skip map[string]bool, team string) bool {
	for l := range skip {
		if strings.HasPrefix(l, team+":") {
			return true
		}
	}
	return false
}

// ensureDetached resolves a detached team to its slot's proxy (a preview starts none); why is the failure.
func ensureDetached(ctx context.Context, c routeCand, skip map[string]bool) (routeCand, string, string) {
	if teamFailed(skip, c.team.Name) {
		return c, "", "failed mid-run"
	}
	if IsDryRun(ctx) {
		return c, PreviewRouterBase(ctx), ""
	}
	id, _ := ctx.Value(dispatchIDKey{}).(string)
	sc, err := ensureTeam(ctx, c.team, id)
	if err != nil {
		return c, "", err.Error()
	}
	c.scope = sc
	return c, proxy.Base(sc.Port), ""
}

// resolveCand is c's base, or why it cannot be used; a detached team's label is known only once resolved.
func resolveCand(ctx context.Context, c routeCand, skip map[string]bool,
	ensure func(context.Context) (string, error)) (routeCand, string, string) {
	var base, why string
	switch {
	case c.team.Host == proxyteam.HostDetached:
		return ensureDetached(ctx, c, skip)
	case skip[c.label()]:
		why = "failed mid-run"
	case c.team.Host == proxyteam.HostDaemon:
		base, err := ensure(ctx)
		if err != nil {
			why = err.Error()
		}
		return c, base, why
	default:
		if why = scopeLive(ctx, c.scope); why == "" {
			base = proxy.Base(c.scope.Port)
		}
	}
	return c, base, why
}

// chooseRoute returns the first live candidate not in skip, its base, and a note when it is not the first.
func chooseRoute(ctx context.Context, agent string, cands []routeCand, skip map[string]bool,
	ensure func(context.Context) (string, error)) (routeCand, string, string, error) {
	var reasons []string
	for i, c := range cands {
		c, base, why := resolveCand(ctx, c, skip, ensure)
		if why != "" {
			reasons = append(reasons, c.label()+": "+why)
			continue
		}
		note := ""
		if i > 0 {
			first := cands[0].label()
			note = fmt.Sprintf("%s: proxy: %s unavailable (%s); using %s",
				agent, first, strings.TrimPrefix(reasons[0], first+": "), c.label())
		}
		return c, base, note, nil
	}
	return routeCand{}, "", "", fmt.Errorf("proxy route walk: %s", strings.Join(reasons, "; "))
}

var (
	routeMu    sync.Mutex
	localBases = map[string]bool{}
	chosenByID = map[string]string{}
)

// TakeChosenProxy returns the route label chosen for dispatch id ("" when none) and forgets it.
func TakeChosenProxy(id string) string {
	routeMu.Lock()
	defer routeMu.Unlock()
	label := chosenByID[id]
	delete(chosenByID, id)
	return label
}

// forgetChosenProxy drops ctx's dispatch label so an attempt that bypasses the proxy names none.
func forgetChosenProxy(ctx context.Context) {
	id, _ := ctx.Value(dispatchIDKey{}).(string)
	routeMu.Lock()
	defer routeMu.Unlock()
	delete(chosenByID, id)
}

// ensureProxyRoute finds the proxy base for ctx: the daemon, or the first live scope proxy of a scoped dispatch.
func ensureProxyRoute(ctx context.Context) (string, error) {
	agent, _ := ctx.Value(scopedProxyKey{}).(string)
	if agent == "" {
		return proxy.Ensure(ctx)
	}
	id, _ := ctx.Value(dispatchIDKey{}).(string)
	cands := routeCands(ctx)
	c, base, note, err := chooseRoute(ctx, agent, cands, failedRoutes(id), proxy.Ensure)
	if err != nil { // every route but the failed ones is dead: the failed ones may have recovered
		c, base, note, err = chooseRoute(ctx, agent, cands, nil, proxy.Ensure)
	}
	if err != nil {
		return "", err
	}
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	NoteProxy(id, c.label())
	routeMu.Lock()
	defer routeMu.Unlock()
	if id != "" {
		if len(lastRouteByID) >= maxRouteMemo { // bound a long-lived host: a finished dispatch never asks again
			clear(lastRouteByID)
			clear(failedByID)
		}
		chosenByID[id], lastRouteByID[id] = c.label(), c.label()
	}
	if c.team.Host == proxyteam.HostDaemon {
		delete(localBases, base) // a closed scope's port may now be the daemon's
		return base, nil
	}
	localBases[base] = true
	return base, nil
}

// failedByID holds the route labels that died mid-run for each dispatch; the relaunch's walk skips them.
var failedByID = map[string]map[string]bool{}

// lastRouteByID is each dispatch's latest chosen route; unlike chosenByID, the run's cleanup never clears it.
var lastRouteByID = map[string]string{}

const maxRouteMemo = 256

func failedRoutes(id string) map[string]bool {
	routeMu.Lock()
	defer routeMu.Unlock()
	return maps.Clone(failedByID[id])
}

func init() {
	retry.KnownProxy = knownProxyAddr
	retry.OnProxyDown = markRouteFailed
}

// markRouteFailed records the route ctx's dispatch last chose as dead, so the next walk starts past it.
func markRouteFailed(ctx context.Context) {
	id, _ := ctx.Value(dispatchIDKey{}).(string)
	routeMu.Lock()
	defer routeMu.Unlock()
	label := lastRouteByID[id]
	if id == "" || label == "" {
		return
	}
	if failedByID[id] == nil {
		failedByID[id] = map[string]bool{}
	}
	failedByID[id][label] = true
}

// knownProxyAddr is true for a loopback host:port that is a registered scope proxy or the daemon.
func knownProxyAddr(hostport string) bool {
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	if localProxyBase(proxy.Base(n)) {
		return true
	}
	st, err := proxy.ReadState() // the recorded daemon port, whether or not it still answers
	return err == nil && st.Port == n
}

// localProxyBase reports whether routerBase (maybe with a /d/ or /via/ suffix) is a registered scope proxy.
func localProxyBase(routerBase string) bool {
	routeMu.Lock()
	defer routeMu.Unlock()
	for b := range localBases {
		if rest, ok := strings.CutPrefix(routerBase, b); ok && (rest == "" || rest[0] == '/') {
			return true
		}
	}
	return false
}

// signLocalToken signs a session token for this process; a scope proxy has no daemon to mint it.
func signLocalToken(ctx context.Context, profiles []string, failover string) (string, error) {
	if len(profiles) == 0 {
		return "", errors.New("local session token: no profiles")
	}
	pid := os.Getpid()
	start, err := proc.StartTime(pid)
	if err != nil {
		return "", fmt.Errorf("local session token: start time: %w", err)
	}
	secret, err := proxy.ClientSecret()
	if err != nil {
		return "", fmt.Errorf("local session token: secret: %w", err)
	}
	f, err := proxy.ConfinementFence(ctx)
	if err != nil {
		return "", fmt.Errorf("local session token: fence: %w", err)
	}
	c := proxy.SessionClaims{Profiles: profiles, Failover: failover, PID: pid, Start: start, Fence: f.Profiles, Open: f.Open}
	tok, err := proxy.SignSessionToken(secret, c, time.Now())
	if err != nil {
		return "", fmt.Errorf("local session token: sign: %w", err)
	}
	return tok, nil
}

// ValidProxyGroup reports whether id can name a phase proxy scope.
func ValidProxyGroup(id string) bool { return proxy.ValidScopePart(id) }

// HostPhaseProxy opens the group team's phase proxy as scope id; internal/proxy/phase sets it, else none is hosted.
var HostPhaseProxy = func(context.Context, string) (string, func()) { return "", func() {} }

// isRouterURL is true for exactly the shared daemon's loopback origin; a scope proxy is chosen by team label, not URL.
func isRouterURL(ctx context.Context, raw string) bool {
	port := proxy.DefaultPort
	if st, ok := proxy.Running(ctx); ok {
		port = st.Port
	}
	return raw == proxy.Base(port)
}

// unitMatchesRow refuses a unit that names another row than p.
func unitMatchesRow(p *providers.Provider, r route.Route) error {
	if r.Provider != "" && r.Provider != p.Provider || r.Profile != "" && r.Profile != route.AutoArm && r.Profile != p.Name {
		return fmt.Errorf("endpoint: the unit names %s, not this row (%s %s)", r.Label(), p.Provider, p.Name)
	}
	return nil
}

// urlMatchesRow refuses a URL with credentials, a query or a fragment, or whose origin is none of the row's own.
func urlMatchesRow(p *providers.Provider, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("endpoint: not a URL")
	}
	if u.User != nil || strings.ContainsAny(raw, "?#") {
		return errors.New("endpoint: want a URL with no credentials, query or fragment")
	}
	for _, d := range p.Dialects() {
		base, _ := p.Route(d)
		if b, err := url.Parse(base); err == nil && b.Scheme == u.Scheme && b.Host == u.Host {
			return nil
		}
	}
	return fmt.Errorf("endpoint: the host %s is not the %s row's own; no credential was sent", u.Host, p.Name)
}
