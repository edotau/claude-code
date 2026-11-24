package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxyteam"
)

type (
	proxyGroupKey  struct{}
	scopedProxyKey struct{}
)

// WithProxyGroup marks ctx with the group id whose phase proxy the dispatch should use.
func WithProxyGroup(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, proxyGroupKey{}, id)
}

// WithScopedProxy makes the launcher walk the team route list for this ctx.
func WithScopedProxy(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopedProxyKey{}, true)
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

// proxyGroupID is the ctx group id, else the id part of HARNESS_DISPATCH_PROXY when its team part is team.
func proxyGroupID(ctx context.Context, team string) string {
	if g, _ := ctx.Value(proxyGroupKey{}).(string); g != "" {
		return g
	}
	if t, id, ok := strings.Cut(os.Getenv("HARNESS_DISPATCH_PROXY"), ":"); ok && t == team {
		return id
	}
	return ""
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
	for _, t := range cfg.Routes(proxyteam.KindOf(facts)) {
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

// ProxyRoutes is the ordered scope list for this dispatch, one entry per team.
func ProxyRoutes(ctx context.Context) []proxy.Scope {
	cands := routeCands(ctx)
	out := make([]proxy.Scope, len(cands))
	for i, c := range cands {
		out[i] = c.scope
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

// chooseRoute returns the first live candidate, its base, and a note when it is not the first.
func chooseRoute(ctx context.Context, cands []routeCand,
	ensure func(context.Context) (string, error)) (routeCand, string, string, error) {
	var reasons []string
	for i, c := range cands {
		var base, why string
		if c.team.Host == proxyteam.HostDaemon {
			b, err := ensure(ctx)
			if err != nil {
				why = err.Error()
			}
			base = b
		} else if why = scopeLive(ctx, c.scope); why == "" {
			base = proxy.Base(c.scope.Port)
		}
		if why != "" {
			reasons = append(reasons, c.label()+": "+why)
			continue
		}
		note := ""
		if i > 0 {
			first := cands[0].label()
			note = fmt.Sprintf("ask: proxy: %s unavailable (%s); using %s",
				first, strings.TrimPrefix(reasons[0], first+": "), c.label())
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
	if scoped, _ := ctx.Value(scopedProxyKey{}).(bool); !scoped {
		return proxy.Ensure(ctx)
	}
	c, base, note, err := chooseRoute(ctx, routeCands(ctx), proxy.Ensure)
	if err != nil {
		return "", err
	}
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	routeMu.Lock()
	defer routeMu.Unlock()
	if id, _ := ctx.Value(dispatchIDKey{}).(string); id != "" {
		chosenByID[id] = c.label()
	}
	if c.team.Host == proxyteam.HostDaemon {
		delete(localBases, base) // a closed scope's port may now be the daemon's
		return base, nil
	}
	localBases[base] = true
	return base, nil
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
	fence, open, err := proxy.ConfinementFence(ctx)
	if err != nil {
		return "", fmt.Errorf("local session token: fence: %w", err)
	}
	c := proxy.SessionClaims{Profiles: profiles, Failover: failover, PID: pid, Start: start, Fence: fence, Open: open}
	tok, err := proxy.SignSessionToken(secret, c, time.Now())
	if err != nil {
		return "", fmt.Errorf("local session token: sign: %w", err)
	}
	return tok, nil
}

// HostPhaseProxy opens the group team's phase proxy as scope id; internal/proxy/phase sets it, else none is hosted.
var HostPhaseProxy = func(context.Context, string) (string, func()) { return "", func() {} }
