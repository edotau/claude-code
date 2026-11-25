// teams.go is the --detail team view: each proxy team, its live proxies, and the dispatches that ran through them.

package tree

import (
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxyteam"
	"github.com/edotau/claude-code/internal/textutil"
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
	Dispatches []string `json:"dispatches,omitempty"`
}

// loadTeams reads the team config, the scope records and the daemon state, and joins the dispatches to them.
func loadTeams(ds []Dispatch) []TeamNode {
	cfg, _ := proxyteam.Load(proxy.TeamOverlay())
	scopes, _ := proxy.ListScopes()
	var daemon *proxy.State
	if st, err := proxy.ReadState(); err == nil {
		daemon = &st
	}
	return buildTeams(cfg, scopes, daemon, ds, procLive)
}

// procLive is false only when pid is gone or its start time changed.
func procLive(pid int, start string) bool {
	got, err := proc.StartTime(pid)
	return err == nil && (start == "" || got == start)
}

// buildTeams gives each team its scoped proxies (the daemon for a shared team) and the dispatches by proxy label.
func buildTeams(cfg proxyteam.Config, scopes []proxy.Scope, daemon *proxy.State, ds []Dispatch, live func(int, string) bool) []TeamNode {
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
			n.Proxies = append(n.Proxies, ProxyNode{PID: daemon.PID, Port: daemon.Port,
				Stale: !live(daemon.PID, ""), Dispatches: byLabel[t.Name]})
		}
		for _, sc := range scopes {
			if sc.Team == t.Name {
				n.Proxies = append(n.Proxies, ProxyNode{ID: sc.ID, PID: sc.PID, Port: sc.Port, Build: sc.Build,
					Stale: !live(sc.PID, sc.Start), Dispatches: byLabel[t.Name+":"+sc.ID]})
			}
		}
		out = append(out, n)
	}
	return out
}

// addTeams adds a "proxy teams" header and each team's rows, every row one line of at most teamRowWidth columns.
func addTeams(l *layout, teams []TeamNode, ds []Dispatch) {
	if len(teams) == 0 {
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
	return s
}
