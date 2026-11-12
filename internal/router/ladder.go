package router

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// EnvCoworker confines every dispatch in the session to <provider>[:<profile>] (vault env or overlay provider).
const EnvCoworker = "HARNESS_COWORKER"

// RouteDatabricks is the provider whose confinement arms one workspace (ArmProfile); any other only filters accounts.
const RouteDatabricks = "databricks"

// ParseCoworker splits a HARNESS_COWORKER value into its provider key and optional profile; "" is no confinement.
func ParseCoworker(v string) (provider, profile string, err error) {
	v = strings.TrimSpace(v)
	provider, profile, named := strings.Cut(v, ":")
	switch {
	case v == "":
		return "", "", nil
	case slices.Contains(providers.ProviderKeys(), provider) && (!named || (profile != "" && !strings.Contains(profile, ":"))):
		return provider, profile, nil
	}
	return "", "", fmt.Errorf("unknown provider (%s)", strings.Join(providers.ProviderKeys(), "|")) // never quotes v
}

// Confined is the non-Databricks confinement HARNESS_COWORKER names: its provider and optional profile; "" outside one.
func Confined() (provider, profile string) {
	provider, profile, err := ParseCoworker(os.Getenv(EnvCoworker))
	if err != nil || provider == RouteDatabricks {
		return "", ""
	}
	return provider, profile
}

// OwnProfiles lists agent's own-login rows under provider, in name order (claude: the anthropic CLI-login rows).
func OwnProfiles(reg *providers.Registry, agent, provider string) []string {
	return slices.DeleteFunc(ownRows(reg, agent), func(n string) bool { return reg.Providers[n].Provider != provider })
}

// CanUse reports agent may run under provider: Databricks serves every agent, any other needs an own-login row there.
func CanUse(reg *providers.Registry, agent, provider string) bool {
	return provider == RouteDatabricks || len(OwnProfiles(reg, agent, provider)) > 0
}

// ArmProfile is the workspace HARNESS_COWORKER=databricks:<p> confines the pick to; "" when the route is anything else.
func ArmProfile() string {
	mode, profile, err := ParseCoworker(os.Getenv(EnvCoworker))
	if err != nil || mode != RouteDatabricks {
		return ""
	}
	return profile
}

// gateLadder drops a candidate the confinement rules out, or whose rung model its account can't serve.
const gateLadder = "ladder"

// ownLoginAccount is a vendor's own login (providers.OwnLoginAccountKeys); a pinned "github·" account also passes.
func ownLoginAccount(account string) bool {
	return (strings.HasPrefix(account, "github·") && account != copilotUnpinned) || slices.Contains(providers.OwnLoginAccountKeys, account)
}

// ladderName is databricks only when HARNESS_COWORKER arms a workspace; any other confinement filters the default ladder.
func ladderName() string {
	if ArmProfile() != "" {
		return providers.LadderDatabricks
	}
	return providers.LadderDefault
}

// pickLadder is the ladder one pick follows: its rungs, which accounts it admits, and claude's slot.
type pickLadder struct {
	rungs       []providers.Rung
	only        string // a non-Databricks confinement's provider: admits only its accounts
	onlyAccount string // provider·profile when that confinement pins a profile
	arm         string // databricks:<arm> admits only that workspace's account and own-login ones
	armAccount  string // ProviderAccount(arm)
	anywhere    bool   // claude scores into the first rung it beats (no rung names claude)
	claudeRungs bool   // a rung names claude, so claude is matched like any agent
}

// ladderFor is the table's ladder for the resolved name; an unset ladder keeps today's score-only order.
func (t *RouteTable) ladderFor() pickLadder {
	name := ladderName()
	l := pickLadder{rungs: t.Ladders[name]}
	if p, profile := Confined(); profile != "" {
		l.only, l.onlyAccount = p, p+"·"+profile
	} else {
		l.only = p
	}
	if name == providers.LadderDatabricks {
		l.arm = ArmProfile()
		l.armAccount = ProviderAccount(l.arm)
	}
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
		if g[0].Agent != "opencode" || lp.l.arm != "" { // the arm runs no free+backup pair
			split = make([][]Candidate, len(g))
			for i, c := range g {
				split[i] = []Candidate{c}
			}
		}
		for _, u := range split {
			if u = lp.l.admit(u, lp.res); len(u) > 0 {
				if rung, cs := lp.placeUnit(u); len(cs) > 0 {
					out = append(out, rungGroup{rung, cs})
				}
			}
		}
	}
	return out
}

// admitGroups applies the confinement filter to each gate group, dropping groups it empties.
func (lp ladderPick) admitGroups(groups [][]Candidate) [][]Candidate {
	var out [][]Candidate
	for _, g := range groups {
		if g = lp.l.admit(g, lp.res); len(g) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// admit drops the unit's candidates the ladder rules out (keeps), recording each as a Skip.
func (l pickLadder) admit(u []Candidate, res *PickResult) []Candidate {
	if l.only == "" && l.arm == "" {
		return u
	}
	return slices.DeleteFunc(slices.Clone(u), func(c Candidate) bool {
		if l.keeps(c.Account) {
			return false
		}
		res.Skipped = append(res.Skipped, Skip{skipLabel(c), gateLadder, l.dropReason()})
		return true
	})
}

// keeps is admit's rule for one account under a filtering ladder.
func (l pickLadder) keeps(account string) bool {
	if l.arm != "" {
		return account == l.armAccount || ownLoginAccount(account)
	}
	return accountProvider(account) == l.only && (l.onlyAccount == "" || account == l.onlyAccount)
}

// dropReason is the Skip reason admit records.
func (l pickLadder) dropReason() string {
	if l.arm != "" {
		return armOutside(l.arm)
	}
	return "outside " + strings.Replace(cmp.Or(l.onlyAccount, l.only), "·", ":", 1)
}

// Outside reports a Skip the confinement filter recorded: the candidate was set aside, never attempted.
func (s Skip) Outside() bool { return s.Gate == gateLadder && strings.HasPrefix(s.Reason, "outside ") }

// armOutside is the Skip reason for an account route databricks:<arm> sets aside.
func armOutside(arm string) string { return "outside " + RouteDatabricks + ":" + arm }

// armSplit moves the wanted agent's off-workspace candidates into the rest: its own login ranks in tier 2 (D1).
func (l pickLadder) armSplit(first, rest [][]Candidate) ([][]Candidate, [][]Candidate) {
	if l.arm == "" {
		return first, rest
	}
	var keep, moved [][]Candidate
	for _, g := range first {
		var on []Candidate
		for _, c := range g {
			if c.Account == l.armAccount {
				on = append(on, c)
			} else {
				moved = append(moved, []Candidate{c})
			}
		}
		if len(on) > 0 {
			keep = append(keep, on)
		}
	}
	return keep, slices.Concat(moved, rest)
}

// placeUnit returns the first rung the unit matches and its candidates (one per rung model); len(rungs) = no rung.
// A unit whose matched rungs all reject its models returns no candidates, so it is never emitted.
func (lp ladderPick) placeUnit(u []Candidate) (int, []Candidate) {
	if lp.l.anywhere && u[0].Agent == "claude" {
		return -1, u
	}
	matched := false
	for i, g := range lp.l.rungs {
		if (g.Agent != "" && g.Agent != u[0].Agent) || (g.Provider != "" && g.Provider != accountProvider(u[0].Account)) {
			continue
		}
		if len(g.Models) == 0 {
			return i, u
		}
		matched = true
		if cs := lp.rungCandidates(g, u); len(cs) > 0 {
			return i, cs
		}
	}
	if matched {
		return len(lp.l.rungs), nil
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

// accountProvider is the part before the first "·": the provider of an account key or the agent of a Skip label.
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
				lp.res.Skipped = append(lp.res.Skipped, Skip{skipLabel(c), gateLadder, why})
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
			if t := u.cs[0].Total; !seen[u.rung] || t > best[u.rung] { // a cooling total can be negative
				best[u.rung] = t
			}
			seen[u.rung] = true
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
