package agent

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

// Gates a candidate must pass before it is scored; a Skip names the one that dropped it.
const (
	GateRow       = "row"
	GateAvailable = "available"
	GateAccount   = "account"
	GateExhausted = "exhausted"
	GateFree      = "free" // a free opencode sub-candidate blocked at opus or on a work repo
	GateOff       = "off"  // the agent is deactivated (<AGENT>_ACTIVE=off)
)

// Candidate is one runnable leg: the runner, its cell at the tier, its account's state and its composite score.
type Candidate struct {
	Agent string `json:"agent"`
	Cell
	State   string             `json:"state"`
	Billing string             `json:"billing"`
	Scores  map[string]float64 `json:"scores,omitempty"`
	Total   float64            `json:"total"`
	// Profile names the profile a pinned or session-anchored candidate runs on; empty for every other candidate.
	Profile string `json:"profile,omitempty"`
}

// Skip is a runner dropped by a gate, with why.
type Skip struct {
	Agent  string `json:"agent"`
	Gate   string `json:"gate"`
	Reason string `json:"reason"`
}

// PickResult is a pick: candidates in run order, the weights that scored them, then every skipped runner.
type PickResult struct {
	Tier       string             `json:"tier"`
	Want       string             `json:"want,omitempty"`
	Weights    map[string]float64 `json:"weights"`
	Candidates []Candidate        `json:"candidates"`
	Skipped    []Skip             `json:"skipped,omitempty"`
}

// PickOpts tunes a pick; nil funcs take the defaults (no presence check, no refresh, time.Now).
type PickOpts struct {
	Fallback   string
	Configured func(runner string, c Cell) error
	// Account resolves the backend a runner actually uses ("" keeps the table's account key) and its billing type.
	Account func(runner string, c Cell) (account, billing string)
	// Refresh re-fetches one stale account and records it; the pick then rereads that entry.
	Refresh func(ctx context.Context, account string) bool
	Now     func() time.Time
	// WorkRepo reports whether the current repo is a configured work repo, so the opencode sub-pick never
	// offers a free Zen candidate there; nil defaults to IsWorkRepo("") (the cwd's git remote), so callers
	// that never heard of this rule still get it for free.
	WorkRepo func() bool
}

// PickScope is what a Dimension may read: the account states and every candidate scored together.
type PickScope struct {
	Table      *RouteTable
	State      router.State
	Candidates []Candidate
}

// Dimension is one scored axis, 0–1 with 1 better; success rate, latency or tier fit can be added beside these.
type Dimension interface {
	Name() string
	Score(c Candidate, s PickScope) float64
}

// Dimensions are the scored axes; table.json's weights must name exactly these.
var Dimensions = []Dimension{priceDim{}, headroomDim{}}

// Pick gates then scores the runners for tier: want first when it passes the gates, then the other vendors by
// total score (low accounts after all non-low), then claude under FallbackAll. claude itself never falls back.
func (t *RouteTable) Pick(ctx context.Context, tier, want string, st router.State, o PickOpts) PickResult {
	now, configured := time.Now(), o.Configured
	if o.Now != nil {
		now = o.Now()
	}
	if configured == nil {
		configured = func(string, Cell) error { return nil }
	}
	if st == nil {
		st = router.State{}
	}
	res := PickResult{Tier: tier, Want: want, Weights: t.Weights}
	pins := AgentPins()
	reg, _ := providers.Load()
	pg := &pickGate{reg: reg, held: map[string]bool{}}
	gate := func(runner string) ([]Candidate, bool) {
		return t.gateCandidate(ctx, pg, tier, runner, pins, st, o, now, configured, &res)
	}
	firstG, restG, lastG := t.gatherCandidates(want, o, gate)
	flatFirst, flatRest, flatLast := flattenGroups(firstG), flattenGroups(restG), flattenGroups(lastG)
	all := t.score(slices.Concat(flatFirst, flatRest, flatLast), st)
	nf, nr := len(flatFirst), len(flatRest)
	firstG = regroup(firstG, all[:nf])
	restG = regroup(restG, all[nf:nf+nr])
	lastG = regroup(lastG, all[nf+nr:])
	// groups sort together (not their individual candidates), so a free winner keeps its paid backup adjacent.
	slices.SortStableFunc(restG, func(a, b []Candidate) int {
		return cmp.Or(cmp.Compare(lowRank(a[0].State), lowRank(b[0].State)), cmp.Compare(b[0].Total, a[0].Total))
	})
	res.Candidates = slices.Concat(flattenGroups(firstG), flattenGroups(restG), flattenGroups(lastG))
	seen := make(map[[3]string]bool)
	res.Candidates = slices.DeleteFunc(res.Candidates, func(c Candidate) bool {
		key := [3]string{c.Agent, c.Account, c.Model}
		if seen[key] {
			return true
		}
		seen[key] = true
		return false
	})
	return res
}

// flattenGroups concatenates each gate's candidate group, in group order.
func flattenGroups(groups [][]Candidate) []Candidate {
	return slices.Concat(groups...)
}

// regroup reslices a flat, scored candidate list back into groups sized like the originals.
func regroup(groups [][]Candidate, flat []Candidate) [][]Candidate {
	out := make([][]Candidate, len(groups))
	for i, g := range groups {
		out[i], flat = flat[:len(g)], flat[len(g):]
	}
	return out
}

// gateRunner runs one runner through the row/configured/account/exhausted gates, recording a Skip on failure.
// It returns a group of candidates from this one runner — one, unless it's opencode's free+backup pair.
func (t *RouteTable) gateRunner(
	ctx context.Context, tier, runner string, st router.State, o PickOpts, now time.Time,
	configured func(runner string, c Cell) error, res *PickResult,
) ([]Candidate, bool) {
	if runner == "opencode" && !t.OpencodeRowOverridden[tier] {
		return t.gateOpencodeCandidate(ctx, tier, "", st, o, now, configured, res)
	}
	c, ok := t.Cell(runner, tier)
	if !ok {
		res.Skipped = append(res.Skipped, Skip{runner, GateRow, "no " + tier + " row"})
		return nil, false
	}
	if err := configured(runner, c); err != nil {
		res.Skipped = append(res.Skipped, Skip{runner, GateAvailable, "not configured: " + firstLine(err.Error())})
		return nil, false
	}
	billing := t.Billing[c.Account]
	if o.Account != nil {
		if a, b := o.Account(runner, c); a != "" {
			c.Account, billing = a, b
		}
	}
	cand, ok := t.gateAccount(ctx, runner, c, billing, st, o, now, res)
	if !ok {
		return nil, false
	}
	return []Candidate{cand}, true
}

// gateAccount runs a resolved cell through the copilot-pin, auth-mark, refresh and exhausted gates; name labels Skips.
func (t *RouteTable) gateAccount(
	ctx context.Context, name string, c Cell, billing string, st router.State, o PickOpts, now time.Time, res *PickResult,
) (Candidate, bool) {
	if c.Account == CopilotUnpinned {
		res.Skipped = append(res.Skipped, Skip{name, GateAccount, "no pinned Copilot account (claude-code configure github)"})
		return Candidate{}, false
	}
	if reason, marked := st.AccountAuthMarked(c.Account, now); marked {
		res.Skipped = append(res.Skipped, Skip{name, router.GateAuth, authSkipReason(reason, c.Account)})
		return Candidate{}, false
	}
	e := st[c.Account]
	if o.Refresh != nil && now.Sub(e.Seen) > router.StaleAfter && e.Status(now) != router.StateExhausted && o.Refresh(ctx, c.Account) {
		e = router.LoadState()[c.Account]
		st[c.Account] = e
	}
	s := e.Status(now)
	if s == router.StateExhausted {
		res.Skipped = append(res.Skipped, Skip{name, GateExhausted, ExhaustedReason(c.Account, e, now)})
		return Candidate{}, false
	}
	return Candidate{Agent: agentOf(name), Cell: c, State: s, Billing: billing}, true
}

// agentOf strips a "<agent>·<sub>" Skip label back to the runner.
func agentOf(name string) string {
	a, _, _ := strings.Cut(name, "·")
	return a
}

// opencodeOrder is the sub-pick's fixed order: the free Zen catalog first, then the ladder's paid backends
// (defaults.json opencodeLadder, vault-overridable) — owner decision, not the composite score, picks the
// backend; price/headroom only break a tie between sub-candidates sharing one order position.
func (t *RouteTable) opencodeOrder() []string {
	return slices.Concat([]string{"zen-free"}, t.OpencodeLadder)
}

// gateOpencodeCandidate runs the opencode sub-pick at tier: zen-free first (skipped at opus or on a work
// repo, BillFree), then each ladder id in order, every one gated on its own login (configured,
// which checks the cell's model prefix) and quota. The first to clear every gate wins; a low account sorts
// after the non-low ones while otherwise keeping this order. ok is false when every sub-candidate was skipped.
// When the winner bills free, the first surviving paid sub-candidate rides along right after it, so a stalled
// free leg has a ready backup without a second opencode pick.
func (t *RouteTable) gateOpencodeCandidate(
	ctx context.Context, tier, only string, st router.State, o PickOpts, now time.Time,
	configured func(runner string, c Cell) error, res *PickResult,
) ([]Candidate, bool) {
	type sub struct {
		id      string
		c       Cell
		billing string
		state   string
	}
	workRepo := o.WorkRepo
	if workRepo == nil {
		workRepo = func() bool { return IsWorkRepo("") }
	}
	var ok []sub
	order := t.opencodeOrder()
	if only != "" {
		order = []string{only}
	}
	for _, id := range order {
		name := "opencode·" + id
		if _, known := t.OpencodeSub[id]; !known {
			res.Skipped = append(res.Skipped, Skip{name, GateRow, "unknown opencode ladder id"})
			continue
		}
		c, has := t.OpencodeSubCell(id, tier)
		if !has {
			continue // e.g. zen-free carries no "opus" row — that is how it stays out of opus, not a tier check
		}
		billing := t.Billing[c.Account]
		if billing == router.BillFree && workRepo() {
			res.Skipped = append(res.Skipped, Skip{name, GateFree, "free blocked on a work repo"})
			continue
		}
		if billing == router.BillFree && !t.OpencodeRouting.FreeTierOK(tier) {
			res.Skipped = append(res.Skipped, Skip{name, GateFree, "free not enabled at " + tier})
			continue
		}
		if err := configured("opencode", c); err != nil {
			res.Skipped = append(res.Skipped, Skip{name, GateAvailable, "not configured: " + firstLine(err.Error())})
			continue
		}
		cand, pass := t.gateAccount(ctx, name, c, billing, st, o, now, res)
		if !pass {
			continue
		}
		ok = append(ok, sub{id, c, billing, cand.State})
	}
	if len(ok) == 0 {
		res.Skipped = append(res.Skipped, Skip{"opencode", GateAvailable, "no opencode sub-candidate passed (no login, exhausted, or free blocked)"})
		return nil, false
	}
	slices.SortStableFunc(ok, func(a, b sub) int { return cmp.Compare(lowRank(a.state), lowRank(b.state)) })
	w := ok[0]
	if only != "" {
		return []Candidate{{Agent: "opencode", Cell: w.c, State: w.state, Billing: w.billing}}, true
	}
	group := []Candidate{{Agent: "opencode", Cell: w.c, State: w.state, Billing: w.billing}}
	if w.billing == router.BillFree {
		for _, s := range ok[1:] {
			if s.billing != router.BillFree {
				group = append(group, Candidate{Agent: "opencode", Cell: s.c, State: s.state, Billing: s.billing})
				break
			}
		}
	}
	return group, true
}

// gatherCandidates runs the wanted runner, then (unless suppressed) the other vendors, then claude under
// FallbackAll. Each gate call's group (usually one candidate, two for a free opencode winner + its paid
// backup) stays together as one element, so a later sort over groups never splits the pair.
func (t *RouteTable) gatherCandidates(want string, o PickOpts, gate func(string) ([]Candidate, bool)) (first, rest, last [][]Candidate) {
	if want != "" {
		if cs, ok := gate(want); ok {
			first = append(first, cs)
		}
	}
	if want != "claude" && o.Fallback != FallbackNone {
		for _, r := range TableRunners {
			if r == want || r == "claude" {
				continue
			}
			if cs, ok := gate(r); ok {
				rest = append(rest, cs)
			}
		}
		if o.Fallback == FallbackAll || o.Fallback == "" {
			if cs, ok := gate("claude"); ok {
				last = append(last, cs)
			}
		}
	}
	return first, rest, last
}

// score fills each candidate's per-dimension scores and weighted total.
func (t *RouteTable) score(cs []Candidate, st router.State) []Candidate {
	sc := PickScope{Table: t, State: st, Candidates: cs}
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
	if s == router.StateLow {
		return 1
	}
	return 0
}

// priceDim: a subscription window already paid for scores its share left; credits score cheapest-at-tier = 1.
type priceDim struct{}

func (priceDim) Name() string { return "price" }

func (priceDim) Score(c Candidate, s PickScope) float64 {
	if c.Billing == router.BillFree {
		return 1
	}
	if c.Billing == router.BillSubscription {
		return headroom(s.State[c.Account])
	}
	p, low := blended(c.Price), -1.0
	for _, o := range s.Candidates {
		if b := blended(o.Price); o.Billing != router.BillSubscription && b > 0 && (low < 0 || b < low) {
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

func (headroomDim) Score(c Candidate, s PickScope) float64 { return headroom(s.State[c.Account]) }

// headroom of an unseen account is 0.5: unknown ranks below a reading known to have room.
func headroom(e router.Entry) float64 {
	if e.Seen.IsZero() {
		return 0.5
	}
	return min(max(1-e.UsedPct/100, 0), 1)
}

// blended weights input 3:1 over output, the usual agentic read/write mix.
func blended(p Price) float64 { return (3*p.In + p.Out) / 4 }

// authSkipReason is the mark's own reason, else the login hint for the account's provider.
func authSkipReason(reason, account string) string {
	if reason != "" {
		return reason
	}
	return "auth rejected: run `claude-code databricks login --profile " + strings.TrimPrefix(account, "databricks·") + "`"
}

// ExhaustedReason names the account and its reset, e.g. "github·aueric_boehcp exhausted until 2026-11-01".
func ExhaustedReason(account string, e router.Entry, now time.Time) string {
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

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
