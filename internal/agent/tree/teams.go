// teams.go is the --detail team view: each proxy team, its live proxies, and the dispatches that ran through them.

package tree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxy/proxyteam"
)

// teamRowWidth is the most columns a team-view row takes, piped or not.
const teamRowWidth = 80

// TeamNode is one team with its proxies, in the `agents tree --format json` schema.
type TeamNode struct {
	Team    proxyteam.Team `json:"team"`
	Proxies []ProxyNode    `json:"proxies,omitempty"`
}

// ProxyNode is one proxy of a team; Dispatches are the ids that ran through it.
type ProxyNode struct {
	ID         string   `json:"id,omitempty"`
	PID        int      `json:"pid"`
	Port       int      `json:"port"`
	Build      string   `json:"build,omitempty"`
	Stale      bool     `json:"stale,omitempty"`
	Unverified bool     `json:"unverified,omitempty"` // the pid lives but no proxy answered on its port
	Dispatches []string `json:"dispatches,omitempty"`
}

// loadTeams joins the team config, scope records and daemon state to the dispatches; warnings flag bad config or state.
func loadTeams(ds []Dispatch) ([]TeamNode, []string) {
	cfg, problems := proxyteam.Load(proxy.TeamOverlay())
	var warns []string
	for _, p := range problems {
		warns = append(warns, "team config: "+p.String())
	}
	scopes, err := proxy.ListScopes()
	if err != nil {
		warns = append(warns, "proxy scopes unreadable: "+err.Error())
	}
	var daemon *proxy.State
	if st, err := proxy.ReadState(); err == nil {
		daemon = &st
	} else if !errors.Is(err, fs.ErrNotExist) {
		warns = append(warns, "proxy.json unreadable: "+err.Error())
	}
	return buildTeams(cfg, scopes, daemon, ds, probeProxy), warns
}

// proxyState is what a probe proved about a recorded proxy.
type proxyState int

const (
	proxyLive proxyState = iota
	proxyStale
	proxyUnverified
)

// probeProxy is stale when pid is gone or its start changed, unverified when it lives without a proxy answering.
func probeProxy(pid int, start string, port int) proxyState {
	got, err := proc.StartTime(pid)
	if err != nil {
		if proc.Alive(pid) {
			return proxyUnverified
		}
		return proxyStale
	}
	if start != "" && got != start {
		return proxyStale
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if proxy.Health(ctx, port) != nil || (start == "" && proxy.HolderComm(pid) != "claude-code") {
		return proxyUnverified
	}
	return proxyLive
}

// withState sets a node's liveness flags from one probe.
func withState(n ProxyNode, st proxyState) ProxyNode {
	n.Stale, n.Unverified = st == proxyStale, st == proxyUnverified
	return n
}

// buildTeams gives each team its scoped proxies (the daemon for a shared team) and the dispatches by proxy label.
func buildTeams(cfg proxyteam.Config, scopes []proxy.Scope, daemon *proxy.State, ds []Dispatch, probe func(pid int, start string, port int) proxyState) []TeamNode {
	byLabel := map[string][]string{}
	for _, d := range ds {
		if d.Proxy != "" {
			byLabel[d.Proxy] = append(byLabel[d.Proxy], d.ID)
		}
	}
	out := make([]TeamNode, 0, len(cfg.Teams))
	for _, t := range cfg.Teams {
		n := TeamNode{Team: t}
		if t.Host == proxyteam.HostDaemon && daemon != nil {
			n.Proxies = append(n.Proxies, withState(ProxyNode{PID: daemon.PID, Port: daemon.Port,
				Dispatches: byLabel[t.Name]}, probe(daemon.PID, "", daemon.Port)))
		}
		for _, sc := range scopes {
			if sc.Team == t.Name {
				n.Proxies = append(n.Proxies, withState(ProxyNode{ID: sc.ID, PID: sc.PID, Port: sc.Port, Build: sc.Build,
					Dispatches: byLabel[t.Name+":"+sc.ID]}, probe(sc.PID, sc.Start, sc.Port)))
			}
		}
		out = append(out, n)
	}
	return out
}

// addTeams adds a "proxy teams" header and each team's rows, every row one line of at most teamRowWidth columns.
func addTeams(l *layout, teams []TeamNode, warns []string, ds []Dispatch) {
	if len(teams) == 0 && len(warns) == 0 {
		return
	}
	tasks := map[string]string{}
	for _, d := range ds {
		tasks[d.ID] = d.Task
	}
	l.sep()
	l.free(0, "proxy teams")
	row := func(indent int, s string) {
		l.free(indent, textutil.TruncateCells(textutil.OneLine(s), teamRowWidth-indent, "…"))
	}
	for _, w := range warns {
		row(2, "! "+w)
	}
	for _, t := range teams {
		row(2, t.Team.Row())
		for _, p := range t.Proxies {
			row(4, "└─ "+proxyText(p))
			for _, id := range p.Dispatches {
				row(7, "· "+id+" "+tasks[id])
			}
		}
	}
}

func proxyText(p ProxyNode) string {
	s := strings.TrimSpace(fmt.Sprintf("%s pid %d :%d", p.ID, p.PID, p.Port)) // a daemon node has no scope id
	if p.Build != "" {
		s += " build " + p.Build
	}
	s += fmt.Sprintf(" · %d in flight", len(p.Dispatches))
	if p.Stale {
		s += " · stale"
	}
	if p.Unverified {
		s += " · unverified"
	}
	return s
}
