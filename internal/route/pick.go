package route

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vendors"
)

// Fallback modes, matching `ask --fallback`: every runner ending in claude, vendors only, or the wanted runner alone.
const (
	FallbackAll     = "all"
	FallbackVendors = "vendors"
	FallbackNone    = "none"
)

// Gates a candidate must pass before it is scored; a Skip names the one that dropped it.
const (
	GateRow       = "row"
	GateAvailable = "available"
	GateAccount   = "account"
	GateExhausted = "exhausted"
)

// Candidate is one runnable leg: the runner, its cell at the tier, its account's state and its composite score.
type Candidate struct {
	Runner string `json:"runner"`
	Cell
	State   string             `json:"state"`
	Billing string             `json:"billing"`
	Scores  map[string]float64 `json:"scores,omitempty"`
	Total   float64            `json:"total"`
}

// Skip is a runner dropped by a gate, with why.
type Skip struct {
	Runner string `json:"runner"`
	Gate   string `json:"gate"`
	Reason string `json:"reason"`
}

// Result is a pick: candidates in run order, the weights that scored them, then every skipped runner.
type Result struct {
	Tier       string             `json:"tier"`
	Want       string             `json:"want,omitempty"`
	Weights    map[string]float64 `json:"weights"`
	Candidates []Candidate        `json:"candidates"`
	Skipped    []Skip             `json:"skipped,omitempty"`
}

// Opts tunes a pick; nil funcs take the defaults (binary/login presence, no refresh, time.Now).
type Opts struct {
	Fallback   string
	Configured func(runner string, c Cell) error
	// Account resolves the backend a runner actually uses ("" keeps the table's account key) and its billing type.
	Account func(runner string, c Cell) (account, billing string)
	// Refresh re-fetches one stale account and records it; the pick then rereads that entry.
	Refresh func(ctx context.Context, account string) bool
	Now     func() time.Time
}

// Scope is what a Dimension may read: the account states and every candidate scored together.
type Scope struct {
	Table      *Table
	State      State
	Candidates []Candidate
}

// Dimension is one scored axis, 0–1 with 1 better; success rate, latency or tier fit can be added beside these.
type Dimension interface {
	Name() string
	Score(c Candidate, s Scope) float64
}

// Dimensions are the scored axes; table.json's weights must name exactly these.
var Dimensions = []Dimension{priceDim{}, headroomDim{}}

// Pick gates then scores the runners for tier: want first when it passes the gates, then the other vendors by
// total score (low accounts after all non-low), then claude under FallbackAll. claude itself never falls back.
func (t *Table) Pick(ctx context.Context, tier, want string, st State, o Opts) Result {
	now, configured := time.Now(), o.Configured
	if o.Now != nil {
		now = o.Now()
	}
	if configured == nil {
		configured = Configured
	}
	if st == nil {
		st = State{}
	}
	res := Result{Tier: tier, Want: want, Weights: t.Weights}
	gate := func(runner string) (Candidate, bool) {
		c, ok := t.Cell(runner, tier)
		if !ok {
			res.Skipped = append(res.Skipped, Skip{runner, GateRow, "no " + tier + " row"})
			return Candidate{}, false
		}
		if err := configured(runner, c); err != nil {
			res.Skipped = append(res.Skipped, Skip{runner, GateAvailable, "not configured: " + firstLine(err.Error())})
			return Candidate{}, false
		}
		billing := t.Billing[c.Account]
		if o.Account != nil {
			if a, b := o.Account(runner, c); a != "" {
				c.Account, billing = a, b
			}
		}
		if c.Account == copilotUnpinned {
			res.Skipped = append(res.Skipped, Skip{runner, GateAccount, "no pinned Copilot account (claude-code configure github)"})
			return Candidate{}, false
		}
		e := st[c.Account]
		if o.Refresh != nil && now.Sub(e.Seen) > StaleAfter && e.Status(now) != StateExhausted && o.Refresh(ctx, c.Account) {
			e = LoadState()[c.Account]
			st[c.Account] = e
		}
		s := e.Status(now)
		if s == StateExhausted {
			res.Skipped = append(res.Skipped, Skip{runner, GateExhausted, ExhaustedReason(c.Account, e, now)})
			return Candidate{}, false
		}
		return Candidate{Runner: runner, Cell: c, State: s, Billing: billing}, true
	}
	var first, rest, last []Candidate
	if want != "" {
		if c, ok := gate(want); ok {
			first = append(first, c)
		}
	}
	if want != "claude" && o.Fallback != FallbackNone {
		for _, r := range Runners {
			if r == want || r == "claude" {
				continue
			}
			if c, ok := gate(r); ok {
				rest = append(rest, c)
			}
		}
		if o.Fallback == FallbackAll || o.Fallback == "" {
			if c, ok := gate("claude"); ok {
				last = append(last, c)
			}
		}
	}
	all := t.score(slices.Concat(first, rest, last), st)
	nf, nr := len(first), len(rest)
	first, rest, last = all[:nf], all[nf:nf+nr], all[nf+nr:]
	slices.SortStableFunc(rest, func(a, b Candidate) int {
		return cmp.Or(cmp.Compare(lowRank(a.State), lowRank(b.State)), cmp.Compare(b.Total, a.Total))
	})
	res.Candidates = slices.Concat(first, rest, last)
	return res
}

// score fills each candidate's per-dimension scores and weighted total.
func (t *Table) score(cs []Candidate, st State) []Candidate {
	sc := Scope{Table: t, State: st, Candidates: cs}
	out := make([]Candidate, len(cs))
	for i, c := range cs {
		c.Scores = map[string]float64{}
		for _, d := range Dimensions {
			v := d.Score(c, sc)
			c.Scores[d.Name()] = v
			c.Total += t.Weights[d.Name()] * v
		}
		out[i] = c
	}
	return out
}

func lowRank(s string) int {
	if s == StateLow {
		return 1
	}
	return 0
}

// priceDim: a subscription window already paid for scores its share left; credits score cheapest-at-tier = 1.
type priceDim struct{}

func (priceDim) Name() string { return "price" }

func (priceDim) Score(c Candidate, s Scope) float64 {
	if c.Billing == BillSubscription {
		return headroom(s.State[c.Account])
	}
	p, low := c.Price.blended(), -1.0
	for _, o := range s.Candidates {
		if b := o.Price.blended(); o.Billing != BillSubscription && b > 0 && (low < 0 || b < low) {
			low = b
		}
	}
	if p <= 0 || low <= 0 {
		return 1 // unpriced cells tie
	}
	return low / p
}

// headroomDim is 1 − the used share of the account's tightest window.
type headroomDim struct{}

func (headroomDim) Name() string { return "headroom" }

func (headroomDim) Score(c Candidate, s Scope) float64 { return headroom(s.State[c.Account]) }

// headroom of an unseen account is 0.5: unknown ranks below a reading known to have room.
func headroom(e Entry) float64 {
	if e.Seen.IsZero() {
		return 0.5
	}
	return min(max(1-e.UsedPct/100, 0), 1)
}

// blended weights input 3:1 over output, the usual agentic read/write mix.
func (p Price) blended() float64 { return (3*p.In + p.Out) / 4 }

// ExhaustedReason names the account and its reset, e.g. "copilot:aueric_boehcp exhausted until 2026-11-01".
func ExhaustedReason(account string, e Entry, now time.Time) string {
	s := account + " exhausted"
	if u := e.Until(now); !u.IsZero() {
		layout := "2006-01-02 15:04"
		if u.Sub(now) > 24*time.Hour {
			layout = "2006-01-02"
		}
		s += " until " + u.Local().Format(layout)
	}
	if e.Reason != "" {
		s += " (" + e.Reason + ")"
	}
	return s
}

// GHAccounts lists the gh logins the copilot gate checks the pin against; a var so tests never exec gh.
var GHAccounts = vendors.GHAccounts

// Configured is the no-network presence check: the vendor binary, plus an opencode login for the cell's provider.
func Configured(runner string, c Cell) error {
	if runner == "claude" {
		return nil
	}
	spec, ok := vendors.Lookup(runner)
	if !ok {
		return fmt.Errorf("unknown runner %s", runner)
	}
	if _, err := paths.VendorBin(spec.Binary); err != nil {
		return err
	}
	if u := copilotUser(); runner == "copilot" && u != "" && !slices.Contains(GHAccounts(), u) {
		return fmt.Errorf("pinned gh user %s is not logged in (gh auth login); no other account is used", u)
	}
	if runner == "opencode" {
		if id, _, found := strings.Cut(c.Model, "/"); found {
			_, err := vendors.OpencodeCredential(id)
			return err
		}
	}
	return nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
