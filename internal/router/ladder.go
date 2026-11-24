package router

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
)

// RouteDatabricks is the provider whose confinement arms one workspace (ArmProfileContext); any other filters accounts.
const RouteDatabricks = "databricks"

// BadOverlay is the provider ConfinedContext reports for an unparseable overlay: no account, so nothing routes.
const BadOverlay = "invalid-overlay"

// unresolvedProfile is the lead profile ConfineLeadContext returns when the registry cannot load: no row has this name.
const unresolvedProfile = "confinement-unresolved"

// OwnProfiles lists agent's own-login rows under provider, in name order (claude: the anthropic CLI-login rows).
func OwnProfiles(reg *providers.Registry, agent, provider string) []string {
	return slices.DeleteFunc(ownRows(reg, agent), func(n string) bool { return reg.Providers[n].Provider != provider })
}

// CanUse reports agent may run under provider: Databricks serves every agent, any other needs an own-login row there.
func CanUse(reg *providers.Registry, agent, provider string) bool {
	return provider == RouteDatabricks || len(OwnProfiles(reg, agent, provider)) > 0
}

// gateLadder drops a candidate the confinement rules out, or whose rung model its account can't serve.
const gateLadder = "ladder"

// ownLoginAccount is a vendor's own login (providers.OwnLoginAccountKeys); a pinned "github·" account also passes.
func ownLoginAccount(account string) bool {
	return (strings.HasPrefix(account, "github·") && account != copilotUnpinned) || slices.Contains(providers.OwnLoginAccountKeys, account)
}

// ladderName is databricks only when the confinement arms a workspace; any other one filters the default ladder.
func ladderName(ctx context.Context) string {
	if ArmProfileContext(ctx) != "" {
		return providers.LadderDatabricks
	}
	return providers.LadderDefault
}

// pickLadder is the ladder one pick follows: its rungs, which accounts it admits, and claude's slot.
type pickLadder struct {
	rungs []providers.Rung
	// a non-Databricks confinement's provider: admits only its accounts
	only string
	// provider·profile when that confinement pins a profile
	onlyAccount string
	// databricks:<arm> admits only that workspace's account and own-login ones
	arm        string
	armAccount string
	// claude scores into the first rung it beats (no rung names claude)
	anywhere bool
	// a rung names claude, so claude is matched like any agent
	claudeRungs bool
}

// ladderFor is the table's ladder for the resolved name; an unset ladder keeps today's score-only order.
func (t *RouteTable) ladderFor(ctx context.Context) pickLadder {
	name := ladderName(ctx)
	l := pickLadder{rungs: t.Ladders[name]}
	if p, profile := ConfinedContext(ctx); profile != "" {
		l.only, l.onlyAccount = p, p+"·"+profile
	} else {
		l.only = p
	}
	if name == providers.LadderDatabricks {
		l.arm = ArmProfileContext(ctx)
		l.armAccount = accountFrom(ctx)(l.arm)
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
		// the arm runs no free+backup pair
		if !agentid.Routing(g[0].Agent).SubLadder || lp.l.arm != "" {
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
func armOutside(arm string) string { return "outside " + route.Join(RouteDatabricks, arm) }

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

// placeUnit is the first rung u matches and its candidates (one per rung model); none when every matched rung rejects.
func (lp ladderPick) placeUnit(u []Candidate) (int, []Candidate) {
	if lp.l.anywhere && u[0].Agent == "claude" {
		return -1, u
	}
	matched := false
	for i, g := range lp.l.rungs {
		if !rungMatches(g, u[0]) {
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
		if agentid.Routing(u[0].Agent).SubLadder {
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
			if !rungKeepsCell(model, c.Model) {
				lp.res.Skipped = append(lp.res.Skipped, Skip{skipLabel(c), gateLadder,
					"rung model " + model + " would replace priced cell " + c.Model})
				continue
			}
			c.Model = model
			out = append(out, c)
		}
	}
	return out
}

// rungKeepsCell reports whether swapping cell for rung keeps its tier (or its family when a tier is unknown).
func rungKeepsCell(rung, cell string) bool {
	if cell == "" || models.SameModel(rung, cell) {
		return true
	}
	if r, c := modelTier(rung), modelTier(cell); r != "" && c != "" {
		return r == c
	}
	return models.FamilyKind(rung) == models.FamilyKind(cell)
}

// modelTier is id's alias tier: its Claude family, else the table.json bucket that lists it; "" when none.
func modelTier(id string) string {
	if f := models.FamilyOf(id); f != "" {
		return f
	}
	for alias, ids := range databricksTiers() {
		if slices.ContainsFunc(ids, func(m string) bool { return models.SameModel(m, id) || InFamily(m, id) }) {
			return alias
		}
	}
	return ""
}

// CandidateRow is c's profile (or the one named by its account) in reg, nil without a registry.
func CandidateRow(reg *providers.Registry, c Candidate) *providers.Provider {
	if reg == nil {
		return nil
	}
	_, profile, _ := strings.Cut(c.Account, "·")
	return reg.Providers[cmp.Or(c.Profile, profile)]
}

// rungModel spells rung model m for c's account row; why is non-empty when that row can't serve it.
func (lp ladderPick) rungModel(c Candidate, m string) (model, why string) {
	p := CandidateRow(lp.reg, c)
	switch {
	case slices.Contains(Aliases, m) && p != nil && p.Models[m] != "":
		return p.Models[m], ""
	case slices.Contains(Aliases, m):
		return lp.aliasCell(c, m)
	case providers.IsFamily(m): // resolved only once the candidate is a dispatch the chain may run
		return m, ""
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

// aliasCell is the table cell's model for alias m on c's agent; why is non-empty when there is none.
func (lp ladderPick) aliasCell(c Candidate, m string) (model, why string) {
	cell, ok := lp.t.Cell(c.Agent, m)
	if !ok {
		return "", "no " + m + " cell for " + c.Agent
	}
	return cell.Model, ""
}

// placeClaude puts claude into the first rung after the leading one whose best total it beats, else after the ladder.
func (l pickLadder) placeClaude(us []rungGroup) {
	best, seen := l.rungBest(us)
	for i := range us {
		if us[i].rung == -1 {
			us[i].rung = l.claudeRung(us[i].cs[0].Total, best, seen)
		}
	}
}

// rungBest is each rung's best unit total, and whether any unit sits on that rung.
func (l pickLadder) rungBest(us []rungGroup) ([]float64, []bool) {
	best := make([]float64, len(l.rungs))
	seen := make([]bool, len(l.rungs))
	for _, u := range us {
		if u.rung >= 0 && u.rung < len(l.rungs) {
			// a cooling total can be negative
			if t := u.cs[0].Total; !seen[u.rung] || t > best[u.rung] {
				best[u.rung] = t
			}
			seen[u.rung] = true
		}
	}
	return best, seen
}

// claudeRung is the first occupied rung after the leading one whose best total claude's beats; len(rungs) for none.
func (l pickLadder) claudeRung(total float64, best []float64, seen []bool) int {
	lead := slices.Index(seen, true)
	for r := range l.rungs {
		if r > lead && seen[r] && total > best[r] {
			return r
		}
	}
	return len(l.rungs)
}

// rungMatches reports rung g admits c's agent and provider; an empty rung field admits any.
func rungMatches(g providers.Rung, c Candidate) bool {
	return (g.Agent == "" || g.Agent == c.Agent) && (g.Provider == "" || g.Provider == accountProvider(c.Account))
}

// sortUnits orders units by rung, then low accounts after the rest, then total score (today's rule within a rung).
func sortUnits(us []rungGroup) {
	slices.SortStableFunc(us, func(a, b rungGroup) int {
		return cmp.Or(cmp.Compare(a.rung, b.rung), cmp.Compare(lowRank(a.cs[0].State), lowRank(b.cs[0].State)),
			cmp.Compare(b.cs[0].Total, a.cs[0].Total))
	})
}

// OverlayRoute is the provider and profile an inline $COWORKER_OVERLAY names; a path or bare databricks gives none.
func OverlayRoute(ctx context.Context) (provider, profile string, err error) {
	if r, ok := ctx.Value(overlayKey{}).(overlayResult); ok {
		return r.provider, r.profile, r.err
	}
	return parseOverlayRoute(ctx)
}

// overlayKey carries one pick's resolved overlay in its context, so the env is parsed once per pick.
type overlayKey struct{}

type overlayResult struct {
	provider, profile string
	err               error
}

func withOverlayRoute(ctx context.Context) context.Context {
	provider, profile, err := parseOverlayRoute(ctx)
	return context.WithValue(ctx, overlayKey{}, overlayResult{provider, profile, err})
}

func parseOverlayRoute(ctx context.Context) (provider, profile string, err error) {
	v := strings.TrimSpace(proc.Value(ctx, route.OverlayVar))
	var doc struct{ Provider, Profile string }
	switch {
	case !strings.HasPrefix(v, "{"):
		return "", "", nil
	case json.Unmarshal([]byte(v), &doc) != nil:
		return "", "", errors.New(route.OverlayVar + ": not valid JSON")
	case doc.Provider == RouteDatabricks && (doc.Profile == "" || doc.Profile == route.AutoArm):
		// the ask applying this document resolves the workspace
		return "", "", nil
	}
	if provider, profile, err = route.Parse(route.Join(doc.Provider, doc.Profile)); err != nil {
		return "", "", fmt.Errorf("%s: %w", route.OverlayVar, err)
	}
	return provider, profile, nil
}

// ConfinedContext is the non-Databricks provider and profile OverlayRoute names; a bad overlay fails closed.
func ConfinedContext(ctx context.Context) (provider, profile string) {
	provider, profile, err := OverlayRoute(ctx)
	switch {
	case err != nil:
		return BadOverlay, ""
	case provider == RouteDatabricks:
		return "", ""
	}
	return provider, profile
}

// ConfineLeadContext fills an untyped lead from a non-Databricks confinement: its pinned profile, else its provider.
func ConfineLeadContext(ctx context.Context, agentName, profile, provider string) (string, string) {
	cp, cprof := ConfinedContext(ctx)
	switch {
	case cp == "" || profile != "":
		return profile, provider
	// a typed --provider (confineFlags: the confinement's own) means its pinned profile, like databricks
	case cprof != "":
		return cprof, ""
	case provider != "":
		return profile, provider
	case agentName != "claude":
		return "", cp
	}
	reg, err := providers.Load()
	// fail closed: a profile no row has, never an unconfined lead
	if err != nil {
		return unresolvedProfile, ""
	}
	// claude reads a profile, never a provider lead
	if own := OwnProfiles(reg, agentName, cp); len(own) > 0 {
		return own[0], ""
	}
	return "", ""
}

// ArmProfileContext is the workspace a databricks:<p> confinement confines the pick to; "" for any other route.
func ArmProfileContext(ctx context.Context) string {
	if mode, profile, err := OverlayRoute(ctx); err == nil && mode == RouteDatabricks {
		return profile
	}
	return ""
}

// ChainReq is one pi runner chain: the head hop's agent, workspace and model, the ask's alias and typed fallback mode.
type ChainReq struct {
	Agent, Profile, Model, Alias, Fallback string
	// Asked is the agent whose own CLI hop 3 runs; empty is claude for a pi head, else the head's own agent
	Asked string
	// TypedModel: the model was named on the command line, so no hop changes it and none leaves the arm
	TypedModel bool
}

// ChainHop is one hop of a chain; OffArm marks the hop that leaves the Databricks arm, taken only under a typed all.
type ChainHop struct {
	Agent, Profile, Account, Model string
	OffArm                         bool
}

// Chain is q's ordered hops and the Skips for hops it could not serve (a model is never swapped, R8).
func (t *RouteTable) Chain(reg *providers.Registry, q ChainReq, now time.Time) ([]ChainHop, []Skip) {
	lp := ladderPick{t: t, reg: reg, alias: q.Alias, now: now, res: &PickResult{}}
	out := lp.addHop(nil, q.Agent, q.Profile, q.Model, false)
	// a rejected head leaves no runnable chain: the caller fails with the Skips, never starts on a sibling
	if len(out) == 0 {
		return nil, lp.res.Skipped
	}
	if q.Fallback == FallbackNone || !slices.Contains(providers.DatabricksProfiles(reg), q.Profile) {
		return out, lp.res.Skipped
	}
	if !agentid.Routing(q.Agent).NoWorkspaceFallback {
		for _, ws := range workspaceRows(reg, q.Profile) {
			out = lp.addHop(out, q.Agent, ws, q.Model, false)
		}
	}
	if q.Fallback != FallbackPin {
		out = lp.ownHops(out, q)
	}
	return out, lp.res.Skipped
}

// ownHops appends hop 3 (own CLI, same profile) and, under a typed all with no typed model, hop 4 off the arm.
func (lp ladderPick) ownHops(out []ChainHop, q ChainReq) []ChainHop {
	own := chainOwn(q)
	if own != q.Agent {
		out = lp.addHop(out, own, q.Profile, cmp.Or(q.Model, q.Alias), false)
	}
	if q.Fallback == FallbackAll && !q.TypedModel {
		for _, row := range OwnProfiles(lp.reg, own, "anthropic") {
			out = lp.addHop(out, own, row, q.Alias, true)
		}
	}
	return out
}

// addHop appends the hop when its row serves model.
func (lp ladderPick) addHop(out []ChainHop, agent, profile, model string, off bool) []ChainHop {
	if h, ok := lp.chainHop(agent, profile, model, off); ok {
		return append(out, h)
	}
	return out
}

// chainOwn is the agent whose own CLI hop 3 runs: Asked, else claude for a pi head, else the head's agent.
func chainOwn(q ChainReq) string {
	if q.Asked != "" {
		return q.Asked
	}
	if q.Agent == agentid.Pi {
		return "claude"
	}
	return q.Agent
}

// chainHop is agent on profile with model, or false and a Skip when the row cannot serve it (never swapped).
func (lp ladderPick) chainHop(agent, profile, model string, off bool) (ChainHop, bool) {
	p := lp.reg.Providers[profile]
	if p == nil {
		lp.res.Skipped = append(lp.res.Skipped, Skip{agent + "·" + profile, gateLadder, "no profile " + profile})
		return ChainHop{}, false
	}
	m, why := lp.rungModel(Candidate{Agent: agent, Account: AccountOf(p), Profile: profile}, model)
	if why == "" {
		m, why = chainModel(agent, p, m, lp.now)
	}
	if why != "" {
		lp.res.Skipped = append(lp.res.Skipped, Skip{agent + "·" + AccountOf(p), gateLadder, why})
		return ChainHop{}, false
	}
	return ChainHop{agent, profile, AccountOf(p), m, off}, true
}

// chainFamily is family m as agent's concrete READY id on p, or why there is none.
func chainFamily(agent string, p *providers.Provider, m string, now time.Time) (model, why string) {
	if m = providers.ConcreteModel(agent, p, m); m == "" {
		return "", "no concrete model for the family on " + p.Name
	}
	if ready, fresh := ServedReady(p, m, now); fresh && !ready {
		return "", "rung model " + m + " is not READY on " + p.Name
	}
	return m, ""
}

// chainModel is m as a concrete id on p (a family resolved), or why agent cannot run it there.
func chainModel(agent string, p *providers.Provider, m string, now time.Time) (model, why string) {
	if providers.IsFamily(m) {
		var why string
		if m, why = chainFamily(agent, p, m, now); why != "" {
			return "", why
		}
	}
	if agentid.Routing(agent).ResponsesRoute && !providers.ResponsesServable(p, m) {
		return "", agent + " needs a Responses model; " + p.Name + " serves " + m + " another way"
	}
	if canon, _, ok := models.Resolve(m); ok && p.Preset == providers.PresetDatabricks && models.FamilyOf(canon) == models.Fable {
		return "", "databricks serves no Fable"
	}
	if f := agentid.Routing(agent).ModelFamily; f != "" && string(models.FamilyKind(m)) != f {
		return "", agent + " does not run " + m
	}
	return m, ""
}
