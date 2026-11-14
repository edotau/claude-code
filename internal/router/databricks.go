package router

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// databricksResponses: the session/pinned profile is a signed-in Databricks row with a Responses route + catalog model.
func databricksResponses(ctx context.Context, reg *providers.Registry, now time.Time, canServe func(string, *providers.Provider) bool) (profile, model string, ok bool) {
	profile, p, ok := sessionDatabricks(ctx, reg, "codex", canServe)
	if !ok {
		return "", "", false
	}
	if first, fresh := ServedFirst(p, true, now); fresh { // a fresh listing proves what the workspace serves
		if first == "" {
			return "", "", false
		}
		return profile, first, true
	}
	if model = providers.FirstResponsesModel(p); model != "" {
		return profile, model, true
	}
	return "", "", false
}

// pickGate is the per-Pick state shared by the gates: the registry, loaded once, and copilot accounts already held.
type pickGate struct {
	reg  *providers.Registry
	held map[string]bool
}

// codexHop is codex's Databricks model and effort at alias: pin, else tier, else cell; non-Responses → first Responses.
func (t *RouteTable) codexHop(ctx context.Context, reg *providers.Registry, alias string, o PickOpts, now time.Time) (profile, model, effort string, ok bool) {
	profile, first, ok := databricksResponses(ctx, reg, now, o.canServe)
	if !ok {
		return "", "", "", false
	}
	cell, _ := t.Cell("codex", alias)
	p := reg.Providers[profile]
	tier := cmp.Or(DatabricksModel("codex", alias, p), cell.Model)
	cell.Model = o.codexModel(providers.Target{Provider: p, Model: tier}).Model
	if wire := p.WireModel(cell.Model); cell.Model != "" && providers.ServesResponses(p, wire) {
		if ready, fresh := ServedReady(p, wire, now); fresh && !ready {
			return profile, first, cell.Effort, true
		}
		return profile, wire, cell.Effort, true
	}
	return profile, first, cell.Effort, true
}

// gateDatabricksCodex gates codex on Databricks; disabled or unusable Databricks returns false without a Skip.
func (t *RouteTable) gateDatabricksCodex(
	ctx context.Context, pg *pickGate, alias string, st State, o PickOpts, now time.Time, configured func(agent string, c Cell) error, res *PickResult,
) (Candidate, bool) {
	reg := pg.reg
	profile, model, effort, ok := t.codexHop(ctx, reg, alias, o, now)
	if !ok {
		return Candidate{}, false
	}
	c := Cell{Model: model, Effort: effort, Account: ProviderAccount(profile)}
	name := "codex·" + c.Account
	if err := configured("codex", c); err != nil {
		skipNotConfigured(res, name, err)
		return Candidate{}, false
	}
	if reason, marked := st.accountAuthMarked(c.Account, now); marked {
		res.Skipped = append(res.Skipped, Skip{name, GateAuth, authSkipReason(reason, c.Account)})
		return Candidate{}, false
	}
	e := st[c.Account]
	if e.Status(now) == StateExhausted {
		res.Skipped = append(res.Skipped, Skip{name, GateExhausted, ExhaustedReason(c.Account, e, now)})
		return Candidate{}, false
	}
	return Candidate{Agent: "codex", Cell: c, State: e.Status(now), Billing: DefaultBilling(reg.Providers[profile]), Profile: profile}, true
}

// gateCandidate gates one agent (off, pinned or unpinned); codex and copilot also offer the session profile.
func (t *RouteTable) gateCandidate(
	ctx context.Context, pg *pickGate, alias, agent string, pins map[string]PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	spec := pins[agent]
	if spec.Off {
		res.Skipped = append(res.Skipped, Skip{agent, GateOff, "deactivated (no profile set)"})
		return nil
	}
	if arm := ArmProfileContext(ctx); arm != "" {
		return t.gateArm(ctx, pg, alias, agent, arm, spec, st, o, now, configured, res)
	}
	group := t.gateBase(ctx, pg, alias, agent, spec, st, o, now, configured, res)
	if agent == "copilot" {
		group = append(group, t.gateSessionCopilot(ctx, pg, alias, "", "", spec, group, st, o, now, configured, res)...)
	}
	if agent == "codex" && spec.Profile == "" {
		group = append(group, t.gateCodexExtras(ctx, pg, alias, group, pins["copilot"], st, o, now, configured, res)...)
	}
	if agent == res.Want && o.Lead != "" && agent != "claude" {
		group = t.gateLead(ctx, pg, alias, agent, spec, group, st, o, now, configured, res)
	}
	return group
}

// gateVendor adds a non-want pinned codex or copilot's own-login rows beside its pin; a skipped pin gets none.
func (t *RouteTable) gateVendor(
	ctx context.Context, pg *pickGate, alias, agent string, own []Candidate, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	if pg.reg == nil || len(own) == 0 || agent == res.Want || (agent != "codex" && agent != "copilot") {
		return nil
	}
	var rows []Candidate
	for _, login := range loginProfiles(pg.reg, agent) {
		if slices.ContainsFunc(own, func(c Candidate) bool { return c.Account == ProviderAccount(login) }) {
			continue
		}
		rows = append(rows, t.gateRow(ctx, pg, alias, agent, login, "", true, st, o, now, configured, res)...)
	}
	return rows
}

// gateRow gates agent on profile prof; an own-login row carries no Profile; quiet keeps only exhausted/auth skips.
func (t *RouteTable) gateRow(
	ctx context.Context, pg *pickGate, alias, agent, prof, model string, quiet bool, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	scratch := &PickResult{Alias: res.Alias, Want: res.Want}
	if quiet && model == DiscoveryModel { // a sibling row is not dispatched to: resolve its pool model without stamping
		model = peekDiscovery(agent, pg.reg.Providers[prof], now)
	}
	g := t.gateProfile(ctx, pg.reg, alias, agent, prof, model, "", st, o, now, configured, scratch)
	for _, sk := range scratch.Skipped {
		if !quiet || sk.Gate == GateExhausted || sk.Gate == GateAuth { // a quiet row reports only real account state
			sk.Reason = strings.Replace(sk.Reason, "pinned profile ", "profile ", 1)
			res.Skipped = append(res.Skipped, sk)
		}
	}
	if p := pg.reg.Providers[prof]; p != nil && p.Auth.Login == agent {
		g = slices.Clone(g)
		for i := range g {
			g[i].Profile = ""
		}
	}
	return g
}

// peekDiscovery is pickPool's choice for p without recording it; "" when pickPool would fall back to the alias.
func peekDiscovery(agent string, p *providers.Provider, now time.Time) string {
	if (agent != "copilot" && agent != "opencode") || p == nil || p.Preset != providers.PresetDatabricks {
		return ""
	}
	st, err := loadState()
	if err != nil {
		return ""
	}
	return leastRecent(st[ProviderAccount(p.Name)], agent, p, servedPool(p, DiscoveryPool(), now), now)
}

// gateLead prepends lead, sibling workspaces and logins as fallback permits; gemini requires Google/gateway rows.
func (t *RouteTable) gateLead(
	ctx context.Context, pg *pickGate, alias, agent string, spec PinSpec, base []Candidate, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	if pg.reg == nil {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "lead " + o.Lead + ": no provider registry"})
		return base
	}
	var lead []Candidate
	rows := leadRows(pg.reg, agent, o)
	for i, prof := range rows {
		if prof == "" || (i > 0 && slices.Contains(rows[:i], prof)) {
			continue
		}
		if agent == "gemini" && !geminiRow(pg.reg, prof) {
			res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "gemini runs on Google or a Databricks gateway row, not " + prof})
			continue
		}
		lead = append(lead, t.gateRow(ctx, pg, alias, agent, prof, spec.Model, i > 0, st, o, now, configured, res)...)
	}
	return mergeLead(lead, base)
}

// gateArm gates agent inside route databricks:<arm>: on arm, then (fallback permitting) on its own logins.
func (t *RouteTable) gateArm(
	ctx context.Context, pg *pickGate, alias, agent, arm string, spec PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	if pg.reg == nil {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, RouteDatabricks + ":" + arm + ": no provider registry"})
		return nil
	}
	own := ownRows(pg.reg, agent)
	armPinSkip(agent, spec.Profile, arm, own, res)
	if agent == "claude" {
		o.CanServe = nil // Serves covers external agents only; claude runs on the workspace and its own login
	}
	var group []Candidate
	if agent != "gemini" || geminiRow(pg.reg, arm) {
		group = t.gateProfile(ctx, pg.reg, alias, agent, arm, spec.Model, "", st, o, now, configured, res)
	} else {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "gemini runs on Google or a Databricks gateway row, not " + arm})
	}
	if o.Fallback == FallbackNone {
		return group
	}
	if agent == "opencode" {
		group = append(group, t.gateOpencodeCandidate(ctx, alias, ArmOpencodeSub, st, o, now, configured, res)...)
	}
	for _, row := range own {
		group = append(group, t.gateProfile(ctx, pg.reg, alias, agent, row, spec.Model, "", st, o, now, configured, res)...)
	}
	return group
}

// ArmOpencodeSub is opencode's own-login sub id, its one own-login hop inside the arm.
const ArmOpencodeSub = "opencode-go"

// armPinSkip records a pin the arm sets aside: a profile or opencode sub id that is neither arm nor agent's own login.
func armPinSkip(agent, pin, arm string, own []string, res *PickResult) {
	if pin == "" || pin == arm || slices.Contains(own, pin) || (agent == "opencode" && pin == ArmOpencodeSub) {
		return
	}
	label := agent + "·" + pin // an opencode sub id is no registry row
	if agent != "opencode" || !slices.Contains(providers.RouteOpencodeSubIDs, pin) {
		label = skipLabel(Candidate{Agent: agent, Account: ProviderAccount(pin)})
	}
	res.Skipped = append(res.Skipped, Skip{label, gateLadder, armOutside(arm)})
}

// ownRows lists agent's own-login rows: loginProfiles, plus for claude the anthropic CLI-login rows.
func ownRows(reg *providers.Registry, agent string) []string {
	rows := loginProfiles(reg, agent)
	if agent != "claude" {
		return rows
	}
	for _, n := range reg.Names() {
		if p := reg.Providers[n]; p.Provider == "anthropic" && p.Auth.Method() == providers.AuthCLI {
			rows = append(rows, n)
		}
	}
	return rows
}

// workspaceRows lists the Databricks workspaces in declared order, minus skip.
func workspaceRows(reg *providers.Registry, skip string) []string {
	return slices.DeleteFunc(slices.Clone(providers.DatabricksProfiles(reg)), func(n string) bool { return n == skip })
}

// gateWorkspaces adds quiet sibling-workspace rows after the wanted Databricks-pinned agent's pin (no lead).
func (t *RouteTable) gateWorkspaces(
	ctx context.Context, pg *pickGate, alias, agent string, spec PinSpec, own []Candidate, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	if pg.reg == nil || len(own) == 0 || agent != res.Want || o.Lead != "" || agent == "gemini" || o.Fallback == FallbackNone ||
		!slices.Contains(providers.DatabricksProfiles(pg.reg), spec.Profile) {
		return nil
	}
	var rows []Candidate
	for _, ws := range workspaceRows(pg.reg, spec.Profile) {
		rows = append(rows, t.gateRow(ctx, pg, alias, agent, ws, spec.Model, true, st, o, now, configured, res)...)
	}
	return rows
}

// loginProfiles lists every registry row carrying agent's own CLI login, in name order.
func loginProfiles(reg *providers.Registry, agent string) []string {
	var rows []string
	for _, n := range reg.Names() {
		if reg.Providers[n].Auth.Login == agent {
			rows = append(rows, n)
		}
	}
	return rows
}

// geminiRow reports a row gemini can run on: Google's own, or a Databricks gateway row named on command.
func geminiRow(reg *providers.Registry, name string) bool {
	p := reg.Providers[name]
	if p == nil {
		return false
	}
	m := p.Auth.Method()
	return p.Provider == "google" || p.Preset == providers.PresetDatabricks && p.Auth.Login == "" && m != providers.AuthNone && m != providers.AuthCLI
}

// gateCodexExtras adds the session Databricks codex candidate and, when codex is wanted, the copilot hop.
func (t *RouteTable) gateCodexExtras(
	ctx context.Context, pg *pickGate, alias string, group []Candidate, copilotSpec PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	var extra []Candidate
	if d, ok := t.gateDatabricksCodex(ctx, pg, alias, st, o, now, configured, res); ok && !slices.ContainsFunc(group, func(c Candidate) bool { return c.Account == d.Account }) {
		extra = append(extra, d)
	}
	if res.Want != "codex" {
		return extra
	}
	if _, model, effort, ok := t.codexHop(ctx, pg.reg, alias, o, now); ok {
		hop := t.gateSessionCopilot(ctx, pg, alias, model, effort, copilotSpec, nil, st, o, now, configured, res)
		for _, c := range hop {
			pg.held[c.Account] = true
		}
		extra = append(extra, hop...)
	}
	return extra
}

// gateSessionCopilot checks copilot on the session's Databricks profile.
func (t *RouteTable) gateSessionCopilot(
	ctx context.Context, pg *pickGate, alias, model, effort string, spec PinSpec, own []Candidate, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	reg := pg.reg
	if spec.Off || reg == nil || !copilotPinOK(reg, spec) {
		return nil
	}
	profile, _, ok := sessionDatabricks(ctx, reg, "copilot", o.canServe)
	if !ok {
		return nil
	}
	account := ProviderAccount(profile)
	if slices.ContainsFunc(own, func(c Candidate) bool { return c.Account == account }) || (own == nil && t.sessionCopilotTaken(pg, alias, account, model, spec, o)) {
		return nil
	}
	return t.gateProfile(ctx, reg, alias, "copilot", profile, model, effort, st, o, now, configured, res)
}

// sessionCopilotTaken reports that account is already held by an earlier gate or is the copilot cell's own account.
func (t *RouteTable) sessionCopilotTaken(pg *pickGate, alias, account, model string, spec PinSpec, o PickOpts) bool {
	return (model == "" && pg.held[account]) || t.copilotCellOn(alias, account, spec, o)
}

// copilotPinOK is true when spec pins no profile, or pins a github one.
func copilotPinOK(reg *providers.Registry, spec PinSpec) bool {
	if spec.Profile == "" {
		return true
	}
	p := reg.Providers[spec.Profile]
	return p != nil && p.Provider == "github"
}

// copilotCellOn reports whether the table's own copilot cell for alias already resolves to account.
func (t *RouteTable) copilotCellOn(alias, account string, spec PinSpec, o PickOpts) bool {
	cell, ok := t.Cell("copilot", alias)
	if !ok {
		return false
	}
	if spec.Profile != "" {
		cell.Account = ProviderAccount(spec.Profile)
	} else if o.Account != nil {
		if a, _ := o.Account("copilot", cell); a != "" {
			cell.Account = a
		}
	}
	return cell.Account == account
}

// sessionDatabricks is the session or pinned profile when it is a signed-in Databricks row agent can use.
func sessionDatabricks(ctx context.Context, reg *providers.Registry, agent string, canServe func(string, *providers.Provider) bool) (profile string, p *providers.Provider, ok bool) {
	if reg == nil {
		return "", nil, false
	}
	profile = providers.SessionLeadContext(ctx, nil, true)
	p = reg.Providers[profile]
	if p == nil || p.Preset != providers.PresetDatabricks || !canServe(agent, p) || !providers.CredentialPresent(p) {
		return "", nil, false
	}
	return profile, p, true
}

func (t *RouteTable) gateBase(
	ctx context.Context, pg *pickGate, alias, agent string, spec PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	var group []Candidate
	if agent != "claude" && spec.Profile != "" {
		group = t.gatePinned(ctx, pg.reg, alias, agent, spec.Profile, spec, st, o, now, configured, res)
		group = append(group, t.gateWorkspaces(ctx, pg, alias, agent, spec, group, st, o, now, configured, res)...)
		group = append(group, t.gateVendor(ctx, pg, alias, agent, group, st, o, now, configured, res)...)
	} else {
		group = t.gateAgent(ctx, alias, agent, st, o, now, configured, res)
		if agent == "gemini" && spec.Model != "" { // native agy login: the GEMINI_MODEL pin beats the table cell
			for i := range group {
				group[i].Model = spec.Model
			}
		}
	}
	return group
}

func leadRows(reg *providers.Registry, agent string, o PickOpts) []string {
	rows := []string{o.Lead}
	if agent != "gemini" && o.Fallback != FallbackNone && slices.Contains(providers.DatabricksProfiles(reg), o.Lead) {
		rows = append(rows, workspaceRows(reg, o.Lead)...)
	}
	rows = append(rows, loginProfiles(reg, agent)...)
	return rows
}

func mergeLead(lead, base []Candidate) []Candidate {
	same := func(a, b Candidate) bool { return a.Agent == b.Agent && a.Account == b.Account && a.Model == b.Model }
	for i, l := range lead {
		if j := slices.IndexFunc(base, func(c Candidate) bool { return same(c, l) }); j >= 0 {
			lead[i] = base[j]
		}
	}
	for _, c := range base {
		if !slices.ContainsFunc(lead, func(l Candidate) bool { return same(l, c) }) {
			lead = append(lead, c)
		}
	}
	return lead
}
