// Package proxyteam is the typed proxy-team schema: closed value sets, load-time validation and total lookups.
package proxyteam

import (
	"slices"
	"strconv"
	"strings"
)

type (
	// Kind is a dispatch's work kind, derived from typed record fields.
	Kind string
	// Host is who runs the team's proxy.
	Host string
	// Lifetime is how long the team's proxy lives.
	Lifetime string
	// Borrow is whether other teams may use the team's proxy.
	Borrow string
)

const (
	KindInteractive Kind = "interactive" // not a dispatch: session, status line, doctor, editors
	KindSingle      Kind = "single"      // a dispatch with no group id and no parent
	KindLane        Kind = "lane"        // its cwd is a linked worktree
	KindNested      Kind = "nested"      // record.parent set
	KindCarried     Kind = "carried"     // record.parent is a carry record
	KindGroup       Kind = "group"       // record.batch_id, plan_task or a group id
	KindMemory      Kind = "memory"      // request.env_names holds a memory job marker
)

const (
	HostDaemon   Host = "daemon"   // the shared daemon on proxy.json
	HostPhase    Host = "phase"    // in-process in the process that owns the phase
	HostDetached Host = "detached" // a standing `gateway proxy --serve --scope` process
	HostAsk      Host = "ask"      // in-process in the ask itself
)

const (
	LifetimeShared   Lifetime = "shared"
	LifetimePhase    Lifetime = "phase"
	LifetimeStanding Lifetime = "standing"
	LifetimeAsk      Lifetime = "ask"
)

const (
	BorrowNever Borrow = "never" // no other team's work is ever sent to it
	BorrowIdle  Borrow = "idle"  // serves another team's overflow while it has no request in flight
	BorrowAny   Borrow = "any"
)

// Kinds lists every Kind; validation and help text read this one list.
func Kinds() []Kind {
	return []Kind{KindInteractive, KindSingle, KindLane, KindNested, KindCarried, KindGroup, KindMemory}
}

// Hosts lists every Host.
func Hosts() []Host { return []Host{HostDaemon, HostPhase, HostDetached, HostAsk} }

// Lifetimes lists every Lifetime.
func Lifetimes() []Lifetime {
	return []Lifetime{LifetimeShared, LifetimePhase, LifetimeStanding, LifetimeAsk}
}

// Borrows lists every Borrow.
func Borrows() []Borrow { return []Borrow{BorrowNever, BorrowIdle, BorrowAny} }

// pairs maps each Host to the one Lifetime it runs with.
var pairs = map[Host]Lifetime{
	HostDaemon: LifetimeShared, HostPhase: LifetimePhase, HostDetached: LifetimeStanding, HostAsk: LifetimeAsk,
}

// MaxReserved caps Team.Reserved: a detached team may start that many proxies, one per slot id.
const MaxReserved = 16

// Team is one row of the schema; every field appears in Fields.
type Team struct {
	Name     string   `json:"name"`
	Kinds    []Kind   `json:"kinds"` // empty only on the default team
	Host     Host     `json:"host"`
	Lifetime Lifetime `json:"lifetime"`
	Reserved int      `json:"reserved"` // proxies kept for the team
	Borrow   Borrow   `json:"borrow"`
	Fallback []string `json:"fallback"`          // team names in order; the last is the shared team
	Default  bool     `json:"default,omitempty"` // takes every kind no team claims
}

// Config is the whole schema document.
type Config struct {
	Schema int    `json:"schema"`
	Teams  []Team `json:"teams"`
}

// Facts are the typed record facts KindOf reads.
type Facts struct{ Interactive, Group, Memory, Carried, Nested, Lane bool }

// Problem is one validation finding; Team and Field are empty when it concerns the whole config.
type Problem struct{ Team, Field, Detail string }

func (p Problem) String() string {
	var parts []string
	for _, s := range []string{p.Team, p.Field} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(append(parts, p.Detail), ": ")
}

// Field is one named value of a Team, in display order.
type Field struct{ Name, Value string }

// Fields lists every Team field in order without reflection (a test pins the count to the struct).
func (t Team) Fields() []Field {
	kinds := make([]string, len(t.Kinds))
	for i, k := range t.Kinds {
		kinds[i] = string(k)
	}
	return []Field{
		{"name", t.Name},
		{"kinds", strings.Join(kinds, ",")},
		{"host", string(t.Host)},
		{"lifetime", string(t.Lifetime)},
		{"reserved", strconv.Itoa(t.Reserved)},
		{"borrow", string(t.Borrow)},
		{"fallback", strings.Join(t.Fallback, ",")},
		{"default", strconv.FormatBool(t.Default)},
	}
}

// KindOf is the first true fact in field order, else KindSingle.
func KindOf(f Facts) Kind {
	switch {
	case f.Interactive:
		return KindInteractive
	case f.Group:
		return KindGroup
	case f.Memory:
		return KindMemory
	case f.Carried:
		return KindCarried
	case f.Nested:
		return KindNested
	case f.Lane:
		return KindLane
	}
	return KindSingle
}

// TeamFor is the team claiming k, else the default team, else the first team, else the zero Team.
func (c Config) TeamFor(k Kind) Team {
	for _, t := range c.Teams {
		if slices.Contains(t.Kinds, k) {
			return t.clone()
		}
	}
	for _, t := range c.Teams {
		if t.Default {
			return t.clone()
		}
	}
	if len(c.Teams) > 0 {
		return c.Teams[0].clone()
	}
	return Team{}
}

func (t Team) clone() Team {
	t.Kinds, t.Fallback = slices.Clone(t.Kinds), slices.Clone(t.Fallback)
	return t
}

// Routes is TeamFor(k), then its fallback teams in order, each once; a valid config ends at the shared team.
func (c Config) Routes(k Kind) []Team {
	first := c.TeamFor(k)
	out, seen := []Team{first}, map[string]bool{first.Name: true}
	add := func(name string) {
		if i := c.index(name); i >= 0 && !seen[name] {
			seen[name] = true
			out = append(out, c.Teams[i].clone())
		}
	}
	for _, n := range first.Fallback {
		add(n)
	}
	for _, t := range c.Teams {
		if t.Lifetime == LifetimeShared {
			add(t.Name)
		}
	}
	return out
}

func (c Config) index(name string) int {
	return slices.IndexFunc(c.Teams, func(t Team) bool { return t.Name == name })
}
