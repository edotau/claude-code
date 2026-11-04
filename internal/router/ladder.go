package router

import (
	"cmp"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// EnvRoute picks the ladder for every dispatch in the session (vault env or `--route`): default or first-party.
const EnvRoute = "HARNESS_ROUTE"

// Route modes `--route` and HARNESS_ROUTE accept; unset picks by the session profile.
const (
	RouteDefault    = "default"
	RouteFirstParty = "first-party"
)

// RouteModes lists the accepted --route values.
var RouteModes = []string{RouteDefault, RouteFirstParty}

// GateLadder drops a candidate the chosen ladder rules out (not first-party, or a rung model the account can't serve).
const GateLadder = "ladder"

// firstPartyAccounts are each vendor's own CLI login on its own subscription; a "github·" account also passes.
var firstPartyAccounts = []string{"anthropic·max", "openai·chatgpt", "google·antigravity", "opencode·go"}

func firstPartyAccount(account string) bool {
	return (strings.HasPrefix(account, "github·") && account != CopilotUnpinned) || slices.Contains(firstPartyAccounts, account)
}

// LadderName is first-party only when HARNESS_ROUTE (`--route` sets it) asks; else default (agent pins decide).
func LadderName() string {
	if strings.TrimSpace(os.Getenv(EnvRoute)) == RouteFirstParty {
		return providers.LadderFirstParty
	}
	return providers.LadderDefault
}

// pickLadder is the ladder one pick follows: its rungs, whether it admits first-party accounts only, and claude's slot.
type pickLadder struct {
	rungs       []providers.Rung
	firstParty  bool
	anywhere    bool // claude scores into the first rung it beats (no rung names claude)
	claudeRungs bool // a rung names claude, so claude is matched like any agent
}

// ladderFor is the table's ladder for the resolved name; an unset ladder keeps today's score-only order.
func (t *RouteTable) ladderFor(reg *providers.Registry) pickLadder {
	name := LadderName()
	l := pickLadder{rungs: t.Ladders[name], firstParty: name == providers.LadderFirstParty && len(t.Ladders[name]) > 0}
	l.claudeRungs = slices.ContainsFunc(l.rungs, func(g providers.Rung) bool { return g.Agent == "claude" })
	l.anywhere = t.ClaudeAnywhere && !l.claudeRungs && len(l.rungs) > 0
	return l
}

// rungGroup is one sortable unit after want: its candidates and the rung they matched (-1 = claude, placed by score).
type rungGroup struct {
	rung int
	cs   []Candidate
}

// ladderPick is one pick's ladder pass: the table, registry, alias and clock it reads, the result it records skips on.
type ladderPick struct {
	t     *RouteTable
	reg   *providers.Registry
	l     pickLadder
	alias string
	now   time.Time
	res   *PickResult
}

// claim moves claude out of the last group when the ladder places it; otherwise last stays last, as before.
func (l pickLadder) claim(rest, last [][]Candidate) ([][]Candidate, [][]Candidate) {
	if !l.anywhere && !l.claudeRungs {
		return rest, last
	}
	return slices.Concat(rest, last), nil
}

// units splits gate groups into per-candidate units (opencode's free+backup pair stays whole), each matched to a rung.
func (lp ladderPick) units(groups [][]Candidate) []rungGroup {
	var out []rungGroup
	for _, g := range groups {
		split := [][]Candidate{g}
		if g[0].Agent != "opencode" {
			split = make([][]Candidate, len(g))
			for i, c := range g {
				split[i] = []Candidate{c}
			}
		}
		for _, u := range split {
			if u = lp.l.admit(u, lp.res); len(u) > 0 {
				rung, cs := lp.placeUnit(u)
				out = append(out, rungGroup{rung, cs})
			}
		}
	}
	return out
}

// admit drops the unit's non-first-party candidates under the firstParty ladder, recording each as a Skip.
func (l pickLadder) admit(u []Candidate, res *PickResult) []Candidate {
	if !l.firstParty {
		return u
	}
	return slices.DeleteFunc(slices.Clone(u), func(c Candidate) bool {
		if firstPartyAccount(c.Account) {
			return false
		}
		res.Skipped = append(res.Skipped, Skip{skipLabel(c), GateLadder, "not first-party"})
		return true
	})
}

// placeUnit returns the first rung the unit matches and its candidates (one per rung model); len(rungs) = no rung.
func (lp ladderPick) placeUnit(u []Candidate) (int, []Candidate) {
	if lp.l.anywhere && u[0].Agent == "claude" {
		return -1, u
	}
	for i, g := range lp.l.rungs {
		if (g.Agent != "" && g.Agent != u[0].Agent) || (g.Provider != "" && g.Provider != accountProvider(u[0].Account)) {
			continue
		}
		if len(g.Models) == 0 {
			return i, u
		}
		if cs := lp.rungCandidates(g, u); len(cs) > 0 {
			return i, cs
		}
	}
	return len(lp.l.rungs), u
}

// skipLabel names a candidate's agent and account once ("codex·databricks·prod", "opencode·zen-free").
func skipLabel(c Candidate) string {
	if strings.HasPrefix(c.Account, c.Agent+"·") {
		return c.Account
	}
	return c.Agent + "·" + c.Account
}

// accountProvider is the provider half of a "<provider>·<profile>" account key.
func accountProvider(account string) string {
	p, _, _ := strings.Cut(account, "·")
	return p
}

// rungCandidates expands a models rung: opencode keeps candidates on the listed subs; others run each model.
func (lp ladderPick) rungCandidates(g providers.Rung, u []Candidate) []Candidate {
	var out []Candidate
	for _, m := range g.Models {
		if u[0].Agent == "opencode" {
			sub := lp.t.OpencodeSub[m][lp.alias].Account
			out = append(out, slices.DeleteFunc(slices.Clone(u), func(c Candidate) bool { return sub == "" || c.Account != sub })...)
			continue
		}
		for _, c := range u {
			model, why := lp.rungModel(c, m)
			if why != "" {
				lp.res.Skipped = append(lp.res.Skipped, Skip{skipLabel(c), GateLadder, why})
				continue
			}
			c.Model = model
			out = append(out, c)
		}
	}
	return out
}

// rungProvider is c's profile (or the one named by its account) in the registry, nil without a registry.
func (lp ladderPick) rungProvider(c Candidate) *providers.Provider {
	if lp.reg == nil {
		return nil
	}
	_, profile, _ := strings.Cut(c.Account, "·")
	return lp.reg.Providers[cmp.Or(c.Profile, profile)]
}

// rungModel spells rung model m for c's account row; why is non-empty when that row can't serve it.
func (lp ladderPick) rungModel(c Candidate, m string) (model, why string) {
	p := lp.rungProvider(c)
	switch {
	case slices.Contains(Aliases, m) && p != nil && p.Models[m] != "":
		return p.Models[m], ""
	case slices.Contains(Aliases, m):
		cell, _ := lp.t.Cell(c.Agent, m)
		return cell.Model, ""
	case p == nil:
		return m, ""
	}
	sp := providers.SpellPin(p, m)
	if sp == "" {
		return "", "rung model " + m + " is not served on " + c.Account
	}
	if ready, fresh := ServedReady(p, sp, lp.now); p.Preset == providers.PresetDatabricks && fresh && !ready {
		return "", "rung model " + sp + " is not READY on " + c.Account
	}
	return sp, ""
}

// placeClaude puts claude into the first rung after the leading one whose best total it beats, else after the ladder.
func (l pickLadder) placeClaude(us []rungGroup) {
	best := make([]float64, len(l.rungs))
	seen := make([]bool, len(l.rungs))
	for _, u := range us {
		if u.rung >= 0 && u.rung < len(l.rungs) {
			best[u.rung], seen[u.rung] = max(best[u.rung], u.cs[0].Total), true
		}
	}
	for i := range us {
		if us[i].rung != -1 {
			continue
		}
		us[i].rung = len(l.rungs)
		lead := slices.Index(seen, true)
		for r := range l.rungs {
			if r > lead && seen[r] && us[i].cs[0].Total > best[r] {
				us[i].rung = r
				break
			}
		}
	}
}

// sortUnits orders units by rung, then low accounts after the rest, then total score (today's rule within a rung).
func sortUnits(us []rungGroup) {
	slices.SortStableFunc(us, func(a, b rungGroup) int {
		return cmp.Or(cmp.Compare(a.rung, b.rung), cmp.Compare(lowRank(a.cs[0].State), lowRank(b.cs[0].State)),
			cmp.Compare(b.cs[0].Total, a.cs[0].Total))
	})
}
