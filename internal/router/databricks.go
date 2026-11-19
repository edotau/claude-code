package router

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/providers"
)

// databricksResponses: the session/pinned profile is a signed-in Databricks row with a Responses route + catalog model.
func databricksResponses(ctx context.Context, reg *providers.Registry, now time.Time, canServe func(string, *providers.Provider) bool) (profile, model string, ok bool) {
	return (&pickGate{reg: reg}).databricksResponses(ctx, now, canServe)
}

func (pg *pickGate) databricksResponses(ctx context.Context, now time.Time, canServe func(string, *providers.Provider) bool) (profile, model string, ok bool) {
	profile, p, ok := pg.sessionDatabricks(ctx, "codex", canServe)
	if !ok {
		return "", "", false
	}
	// a fresh listing proves what the workspace serves
	if first, fresh := ServedFirst(p, true, now); fresh {
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

// pickGate keeps registry, session and account lookups local to one pick.
type pickGate struct {
	reg         *providers.Registry
	held        map[string]bool
	accounts    map[string]string
	session     string
	sessionRead bool
	credentials map[string]bool
}

// codexHop is codex's Databricks model and effort at alias: pin, else tier, else cell; non-Responses → first Responses.
func (t *RouteTable) codexHop(ctx context.Context, pg *pickGate, alias string, o PickOpts, now time.Time) (profile, model, effort string, ok bool) {
	reg := pg.reg
	profile, first, ok := pg.databricksResponses(ctx, now, o.canServe)
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
	profile, model, effort, ok := t.codexHop(ctx, pg, alias, o, now)
	if !ok {
		return Candidate{}, false
	}
	c := Cell{Model: model, Effort: effort, Account: pg.providerAccount(profile)}
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
		if slices.ContainsFunc(own, func(c Candidate) bool { return c.Account == pg.providerAccount(login) }) {
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
	g := t.gateProfile(ctx, pg.reg, alias, agent, prof, model, "", st, o, now, configured, scratch)
	for _, sk := range scratch.Skipped {
		// a quiet row reports only real account state
		if !quiet || sk.Gate == GateExhausted || sk.Gate == GateAuth {
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

// gateLead prepends lead, sibling workspaces and logins as fallback permits; gemini requires Google or Databricks rows.
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
		if agentid.Routing(agent).GoogleOrDatabricks && !geminiRow(pg.reg, prof) {
			res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "gemini runs on Google or a Databricks row, not " + prof})
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
	pg.armPinSkip(agent, spec.Profile, arm, own, res)
	if agentid.Routing(agent).AnthropicRoute {
		// Serves covers external agents only; claude runs on the workspace and its own login
		o.CanServe = nil
	}
	var group []Candidate
	if !agentid.Routing(agent).GoogleOrDatabricks || geminiRow(pg.reg, arm) {
		group = t.gateProfile(ctx, pg.reg, alias, agent, arm, spec.Model, "", st, o, now, configured, res)
	} else {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "gemini runs on Google or a Databricks row, not " + arm})
	}
	if o.oneHop() {
		return group
	}
	if agentid.Routing(agent).SubLadder {
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
func (pg *pickGate) armPinSkip(agent, pin, arm string, own []string, res *PickResult) {
	if pin == "" || pin == arm || slices.Contains(own, pin) || (agentid.Routing(agent).SubLadder && pin == ArmOpencodeSub) {
		return
	}
	// an opencode sub id is no registry row
	label := agent + "·" + pin
	if !agentid.Routing(agent).SubLadder || !slices.Contains(providers.RouteOpencodeSubIDs, pin) {
		label = skipLabel(Candidate{Agent: agent, Account: pg.providerAccount(pin)})
	}
	res.Skipped = append(res.Skipped, Skip{label, gateLadder, armOutside(arm)})
}

// ownRows lists agent's own-login rows: loginProfiles, plus for claude the anthropic CLI-login rows.
func ownRows(reg *providers.Registry, agent string) []string {
	rows := loginProfiles(reg, agent)
	if !agentid.Routing(agent).AnthropicRoute {
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
	if pg.reg == nil || len(own) == 0 || agent != res.Want || o.Lead != "" || agentid.Routing(agent).NoWorkspaceFallback || o.oneHop() ||
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

// geminiRow reports a row gemini can run on: Google's own, or a Databricks row named on command.
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
	if _, model, effort, ok := t.codexHop(ctx, pg, alias, o, now); ok {
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
	profile, _, ok := pg.sessionDatabricks(ctx, "copilot", o.canServe)
	if !ok {
		return nil
	}
	account := pg.providerAccount(profile)
	if slices.ContainsFunc(own, func(c Candidate) bool { return c.Account == account }) || (own == nil && t.sessionCopilotTaken(pg, alias, account, model, spec, o)) {
		return nil
	}
	return t.gateProfile(ctx, reg, alias, "copilot", profile, model, effort, st, o, now, configured, res)
}

// sessionCopilotTaken reports that account is already held by an earlier gate or is the copilot cell's own account.
func (t *RouteTable) sessionCopilotTaken(pg *pickGate, alias, account, model string, spec PinSpec, o PickOpts) bool {
	return (model == "" && pg.held[account]) || t.copilotCellOn(pg, alias, account, spec, o)
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
func (t *RouteTable) copilotCellOn(pg *pickGate, alias, account string, spec PinSpec, o PickOpts) bool {
	cell, ok := t.Cell("copilot", alias)
	if !ok {
		return false
	}
	if spec.Profile != "" {
		cell.Account = pg.providerAccount(spec.Profile)
	} else if o.Account != nil {
		if a, _ := o.Account("copilot", cell); a != "" {
			cell.Account = a
		}
	}
	return cell.Account == account
}

// sessionDatabricks is the session or pinned profile when it is a signed-in Databricks row agent can use.
func (pg *pickGate) sessionDatabricks(ctx context.Context, agent string, canServe func(string, *providers.Provider) bool) (profile string, p *providers.Provider, ok bool) {
	if pg.reg == nil {
		return "", nil, false
	}
	if !pg.sessionRead {
		pg.session = providers.SessionLeadContext(ctx, nil, true)
		pg.sessionRead = true
	}
	profile = pg.session
	p = pg.reg.Providers[profile]
	if p == nil || p.Preset != providers.PresetDatabricks || !canServe(agent, p) || !pg.credentialPresent(p) {
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
		group = append(group, t.gateRecordFallback(ctx, pg, alias, agent, spec, group, st, o, now, configured, res)...)
		group = append(group, t.gateWorkspaces(ctx, pg, alias, agent, spec, group, st, o, now, configured, res)...)
		group = append(group, t.gateVendor(ctx, pg, alias, agent, group, st, o, now, configured, res)...)
	} else {
		group = t.gateAgent(ctx, alias, agent, st, o, now, configured, res)
		group = append(group, t.gateRecordFallback(ctx, pg, alias, agent, spec, group, st, o, now, configured, res)...)
		// native agy login: the GEMINI_MODEL pin beats the table cell
		if agent == "gemini" && spec.Model != "" {
			for i := range group {
				group[i].Model = spec.Model
			}
		}
	}
	return group
}

// gateRecordFallback gates the record's fallback row when nothing else gated (D2/D3), without the pin's model.
func (t *RouteTable) gateRecordFallback(
	ctx context.Context, pg *pickGate, alias, agent string, spec PinSpec, group []Candidate, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	fb := spec.Fallback
	if agent == "claude" || len(group) > 0 || fb == "" || fb == spec.Profile || o.Fallback == FallbackNone || o.Fallback == "" {
		return nil
	}
	return t.gatePinned(ctx, pg.reg, alias, agent, fb, PinSpec{Profile: fb}, st, o, now, configured, res)
}

func leadRows(reg *providers.Registry, agent string, o PickOpts) []string {
	rows := []string{o.Lead}
	if !agentid.Routing(agent).NoWorkspaceFallback && !o.oneHop() && slices.Contains(providers.DatabricksProfiles(reg), o.Lead) {
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

func (pg *pickGate) providerAccount(profile string) string {
	if account, ok := pg.accounts[profile]; ok {
		return account
	}
	if pg.accounts == nil {
		pg.accounts = make(map[string]string)
	}
	account := ProviderAccount(profile)
	pg.accounts[profile] = account
	return account
}

func (pg *pickGate) credentialPresent(p *providers.Provider) bool {
	if present, ok := pg.credentials[p.Name]; ok {
		return present
	}
	if pg.credentials == nil {
		pg.credentials = make(map[string]bool)
	}
	present := providers.CredentialPresent(p)
	pg.credentials[p.Name] = present
	return present
}
