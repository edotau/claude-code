package router

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

// databricksResponses: the session/pinned profile is a signed-in Databricks row with a Responses route + catalog model.
func databricksResponses(reg *providers.Registry, now time.Time, canServe func(string, *providers.Provider) bool) (profile, model string, ok bool) {
	profile, p, ok := sessionDatabricks(reg, "codex", canServe)
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

// codexHop is the model and effort codex's Databricks hops run for alias: codex's own pick respelled for the
// profile when it serves Responses, else the profile's first Responses model.
func (t *RouteTable) codexHop(reg *providers.Registry, alias string, o PickOpts, now time.Time) (profile, model, effort string, ok bool) {
	profile, first, ok := databricksResponses(reg, now, o.canServe)
	if !ok {
		return "", "", "", false
	}
	cell, _ := t.Cell("codex", alias)
	p := reg.Providers[profile]
	cell.Model = o.codexModel(providers.Target{Provider: p, Model: cell.Model}).Model
	if wire := p.WireModel(cell.Model); cell.Model != "" && providers.ServesResponses(p, wire) {
		if ready, fresh := ServedReady(p, wire, now); fresh && !ready {
			return profile, first, cell.Effort, true
		}
		return profile, wire, cell.Effort, true
	}
	return profile, first, cell.Effort, true
}

// gateDatabricksCodex is codex's Databricks candidate at alias: the same configured/auth/exhausted gates as any
// agent, on account databricks·<profile>. ok is false (no Skip) when Databricks is off or serves no usable model.
func (t *RouteTable) gateDatabricksCodex(
	pg *pickGate, alias string, st State, o PickOpts, now time.Time, configured func(agent string, c Cell) error, res *PickResult,
) (Candidate, bool) {
	reg := pg.reg
	profile, model, effort, ok := t.codexHop(reg, alias, o, now)
	if !ok {
		return Candidate{}, false
	}
	c := Cell{Model: model, Effort: effort, Account: ProviderAccount(profile)}
	name := "codex·" + c.Account
	if err := configured("codex", c); err != nil {
		res.Skipped = append(res.Skipped, Skip{name, GateAvailable, "not configured: " + textutil.FirstLine(strings.TrimSpace(err.Error()))})
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
	billing := BillSubscription
	if reg.Providers[profile].APIKeyAuth() {
		billing = BillCredits
	}
	return Candidate{Agent: "codex", Cell: c, State: e.Status(now), Billing: billing, Profile: profile}, true
}

// gateCandidate gates one agent (off, pinned or unpinned); codex and copilot also offer candidates on the session profile.
func (t *RouteTable) gateCandidate(
	ctx context.Context, pg *pickGate, alias, agent string, pins map[string]PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) ([]Candidate, bool) {
	spec := pins[agent]
	if spec.Off {
		res.Skipped = append(res.Skipped, Skip{agent, GateOff, "deactivated (no profile set)"})
		return nil, false
	}
	if arm := ArmProfile(); arm != "" {
		return t.gateArm(ctx, pg, alias, agent, arm, spec, st, o, now, configured, res)
	}
	var group []Candidate
	if agent != "claude" && spec.Profile != "" {
		group, _ = t.gatePinned(ctx, pg.reg, alias, agent, spec.Profile, spec, st, o, now, configured, res)
		group = append(group, t.gateWorkspaces(ctx, pg, alias, agent, spec, group, st, o, now, configured, res)...)
		group = append(group, t.gateVendor(ctx, pg, alias, agent, group, st, o, now, configured, res)...)
	} else {
		group, _ = t.gateAgent(ctx, alias, agent, st, o, now, configured, res)
		if agent == "gemini" && spec.Model != "" { // native agy login: the GEMINI_MODEL pin beats the table cell
			for i := range group {
				group[i].Model = spec.Model
			}
		}
	}
	if agent == "copilot" {
		group = append(group, t.gateSessionCopilot(ctx, pg, alias, "", "", spec, group, st, o, now, configured, res)...)
	}
	if agent == "codex" && spec.Profile == "" {
		group = append(group, t.gateCodexExtras(ctx, pg, alias, group, pins["copilot"], st, o, now, configured, res)...)
	}
	if agent == res.Want && o.Lead != "" && agent != "claude" {
		group = t.gateLead(ctx, pg, alias, agent, spec, group, st, o, now, configured, res)
	}
	return group, len(group) > 0
}

// gateVendor (non-want agents only; the want path is unchanged without Lead) is a pinned codex or copilot's own-login rows beside its pin; a skipped pin gets none (fails closed).
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

// gateRow gates agent on profile prof. An own-login row carries no Profile, as on the unpinned path; quiet keeps only exhausted/auth skips.
func (t *RouteTable) gateRow(
	ctx context.Context, pg *pickGate, alias, agent, prof, model string, quiet bool, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	scratch := &PickResult{Alias: res.Alias, Want: res.Want}
	g, _ := t.gateProfile(ctx, pg.reg, alias, agent, prof, model, "", st, o, now, configured, scratch)
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

// gateLead puts the agent on o.Lead, then (fallback permitting) sibling workspaces, then login rows, ahead of base (base wins).
// gemini never runs on a non-Google row (owner rule R1).
func (t *RouteTable) gateLead(
	ctx context.Context, pg *pickGate, alias, agent string, spec PinSpec, base []Candidate, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) []Candidate {
	if pg.reg == nil {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "lead " + o.Lead + ": no provider registry"})
		return base
	}
	var lead []Candidate
	rows := []string{o.Lead}
	if agent != "gemini" && o.Fallback != FallbackNone && slices.Contains(providers.DatabricksProfiles(pg.reg), o.Lead) {
		rows = append(rows, workspaceRows(pg.reg, o.Lead)...)
	}
	rows = append(rows, loginProfiles(pg.reg, agent)...)
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

// gateArm gates agent inside route databricks:<arm>: on arm, then (fallback permitting) on its own first-party logins.
func (t *RouteTable) gateArm(
	ctx context.Context, pg *pickGate, alias, agent, arm string, spec PinSpec, st State, o PickOpts, now time.Time,
	configured func(agent string, c Cell) error, res *PickResult,
) ([]Candidate, bool) {
	if pg.reg == nil {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, RouteDatabricks + ":" + arm + ": no provider registry"})
		return nil, false
	}
	own := firstPartyRows(pg.reg, agent)
	armPinSkip(agent, spec.Profile, arm, own, res)
	if agent == "claude" {
		o.CanServe = nil // Serves covers external agents only; claude runs on the workspace and its own login
	}
	var group []Candidate
	if agent != "gemini" || geminiRow(pg.reg, arm) {
		group, _ = t.gateProfile(ctx, pg.reg, alias, agent, arm, spec.Model, "", st, o, now, configured, res)
	} else {
		res.Skipped = append(res.Skipped, Skip{agent, GateAvailable, "gemini runs on Google or a Databricks gateway row, not " + arm})
	}
	if o.Fallback == FallbackNone {
		return group, len(group) > 0
	}
	if agent == "opencode" {
		g, _ := t.gateOpencodeCandidate(ctx, alias, ArmOpencodeSub, st, o, now, configured, res)
		group = append(group, g...)
	}
	for _, row := range own {
		g, _ := t.gateProfile(ctx, pg.reg, alias, agent, row, spec.Model, "", st, o, now, configured, res)
		group = append(group, g...)
	}
	return group, len(group) > 0
}

// ArmOpencodeSub is opencode's own-login sub id, its one first-party hop inside the arm.
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

// firstPartyRows lists agent's own-login rows: loginProfiles, plus for claude the anthropic CLI-login rows.
func firstPartyRows(reg *providers.Registry, agent string) []string {
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

// gateWorkspaces adds quiet sibling-workspace rows after the wanted Databricks-pinned agent's pin when no lead reorders it.
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

// geminiRow reports a row gemini can run on: Google first-party, or a Databricks gateway row named on command.
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
	if d, ok := t.gateDatabricksCodex(pg, alias, st, o, now, configured, res); ok && !slices.ContainsFunc(group, func(c Candidate) bool { return c.Account == d.Account }) {
		extra = append(extra, d)
	}
	if res.Want != "codex" {
		return extra
	}
	if _, model, effort, ok := t.codexHop(pg.reg, alias, o, now); ok {
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
	profile, _, ok := sessionDatabricks(reg, "copilot", o.canServe)
	if !ok {
		return nil
	}
	account := ProviderAccount(profile)
	if slices.ContainsFunc(own, func(c Candidate) bool { return c.Account == account }) || (own == nil && t.sessionCopilotTaken(pg, alias, account, model, spec, o)) {
		return nil
	}
	group, _ := t.gateProfile(ctx, reg, alias, "copilot", profile, model, effort, st, o, now, configured, res)
	return group
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
func sessionDatabricks(reg *providers.Registry, agent string, canServe func(string, *providers.Provider) bool) (profile string, p *providers.Provider, ok bool) {
	if reg == nil {
		return "", nil, false
	}
	profile = providers.SessionLead(nil, true)
	p = reg.Providers[profile]
	if p == nil || p.Preset != providers.PresetDatabricks || !canServe(agent, p) || !providers.CredentialPresent(p) {
		return "", nil, false
	}
	return profile, p, true
}
