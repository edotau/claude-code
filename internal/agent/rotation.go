// rotation.go: auto's rotation pool, served models on the agent's rows that meet the tier's ranks.json rule.
package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/copilotauth"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	endpoints "github.com/edotau/claude-code/internal/serving"
	"github.com/edotau/claude-code/internal/vault"
)

// poolOf is the lazy any-tier rotation pool of agentName on p, for router's pool checks.
func poolOf(agentName string, p *providers.Provider) func() []string {
	return func() []string { return rotationPool(agentName, "", p, false, time.Now()) }
}

type peekKey struct{}

// PeekContext marks ctx's route resolution as a label: a rotation pick is peeked, never recorded.
func PeekContext(ctx context.Context) context.Context { return context.WithValue(ctx, peekKey{}, true) }

// Peeking reports whether ctx carries PeekContext's mark.
func Peeking(ctx context.Context) bool { v, _ := ctx.Value(peekKey{}).(bool); return v }

// rotationTiers maps a route alias to the ranks.json rotation tier whose floor and cost cap it takes.
var rotationTiers = map[string]string{models.Haiku: "fast", models.Sonnet: "standard", models.Opus: "deep"}

// rotationPool is the served models agent runs on p that meet alias's rotation rule; "" alias is every tier's union.
func rotationPool(agentName, alias string, p *providers.Provider, tools bool, now time.Time) []string {
	reg, err := providers.LoadShared()
	if err != nil || !router.RotationRow(p) {
		return nil
	}
	cands := withoutDeprecated(p, memoCandidates(reg, agentName, p, tools, now))
	tiers := []string{"fast", "standard", "deep"}
	if t := rotationTiers[alias]; t != "" {
		tiers = []string{t}
	}
	var out []string
	for _, t := range tiers {
		for _, m := range catalog.RotationPool(cands, t, tools) {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	if allow, ok := router.PoolOverlay(); ok {
		out = slices.DeleteFunc(out, func(m string) bool {
			return !slices.ContainsFunc(allow, func(a string) bool { return providers.SpellPin(p, a) == m })
		})
	}
	return out
}

// withoutDeprecated drops candidates on p's dropped list (discovery's, or the proxy's deprecated-400 record).
func withoutDeprecated(p *providers.Provider, cands []catalog.Candidate) []catalog.Candidate {
	dropped, _ := catalog.DroppedIDs(p)
	if len(dropped) == 0 {
		return cands
	}
	return slices.DeleteFunc(cands, func(c catalog.Candidate) bool { return dropped[catalog.ID(c.Model)] })
}

// rotationCandidates is p's served chat models agent runs: a Databricks listing, else the Copilot catalog snapshot.
func rotationCandidates(reg *providers.Registry, agentName string, p *providers.Provider, tools bool, now time.Time) []catalog.Candidate {
	if p.Preset == providers.PresetDatabricks {
		menu := providers.OpencodeMenu()
		cands, _ := autoCandidates(reg, ModelQuery{Agent: agentName, Tools: tools, menu: &menu}, p, now)
		if agentName == agentid.OpenCode {
			return opencodeRunnable(p, cands)
		}
		return driverRunnable(agentName, p, cands)
	}
	var out []catalog.Candidate
	var probes map[string]serving.CheckResult
	if tools {
		probes, _ = catalog.LoadCheckCache(p.Name)
	}
	for _, id := range copilotauth.IDs(p) {
		if catalog.ID(id) != AutoModel { // Copilot's own auto router is no model to rotate onto
			out = append(out, catalog.Candidate{Model: id, Tools: probes[catalog.ID(id)].Tools})
		}
	}
	return out
}

// opencodeRunnable keeps candidates opencode lists on p (base menu or a family id) with a route for the wire.
func opencodeRunnable(p *providers.Provider, cands []catalog.Candidate) []catalog.Candidate {
	listed := map[string]bool{}
	for _, m := range providers.OpencodeMenu() {
		listed[agentconfig.MenuWire(p, m.ID)] = true
	}
	return slices.DeleteFunc(cands, func(c catalog.Candidate) bool {
		_, _, member := providers.FamilyVersion(c.Model)
		_, routed, _ := providers.WireFor(p, c.Model)
		return !listed[c.Model] && !member || !routed
	})
}

// PoolRunnable is an optional Driver method: whether the agent can run model on p, so auto's pool admits only those.
type PoolRunnable interface {
	Runnable(p *providers.Provider, model string) bool
}

func driverRunnable(agentName string, p *providers.Provider, cands []catalog.Candidate) []catalog.Candidate {
	d, ok := driverFor(agentName)
	pr, can := d.(PoolRunnable)
	if !ok || !can {
		return cands
	}
	return slices.DeleteFunc(cands, func(c catalog.Candidate) bool { return !pr.Runnable(p, c.Model) })
}

// rotatesUnderAuto is false for claude: an asked auto keeps the brief's tier model on the one profile it runs on.
func rotatesUnderAuto(agentName string) bool { return !VaultAgentIsClaude(agentName) }

// untieredAuto is an asked auto's alias for a brief with no tier: opus for a rotating agent, else sonnet.
func untieredAuto(agentName string) string {
	if rotatesUnderAuto(agentName) {
		return models.Opus
	}
	return models.Sonnet
}

// roleNote is a role rotation's preflight note and the account its pick should lead on; zero when it did not rotate.
type roleNote struct{ text, account string }

// onto is c with the note as a preflight line; a route whose first hop left the picked account says so.
func (n roleNote) onto(c Chain) Chain {
	if n.text == "" || len(c.Dispatches) == 0 {
		return c
	}
	text := n.text
	if n.account != "" && c.Dispatches[0].Account != n.account {
		text += "; the picked row did not lead, the route runs " + hopTarget(c.Dispatches[0])
	}
	return c.noted(text)
}

// rotateRole runs an untyped Claude dispatch on its role's least-recently-picked ready cell; the note says so.
func rotateRole(ctx context.Context, o RunOptions) (context.Context, RunOptions, roleNote, error) {
	if !untypedClaude(ctx, o) {
		return replayRotation(ctx, o), o, roleNote{}, nil
	}
	reg, err := providers.LoadShared()
	if err != nil {
		return ctx, o, roleNote{}, nil
	}
	ov, err := agentconfig.LoadOverlayContext(ctx, "", nil)
	if err != nil || overlayRoutes(ov) {
		return ctx, o, roleNote{}, nil
	}
	now, role := time.Now(), dispatchRole(o.Prompt)
	// where today's untyped route starts (session, record profile, default): no ready cell keeps it, never an excluded one
	session := cmp.Or(providers.SessionLeadContext(ctx, reg, true), reg.Default)
	cells, scope := modeCells(reg, ov.ProviderMode, role, session)
	eng, _ := Lookup(o.Agent) // a missing engine fails at Plan's own Lookup right after
	scan := cellScan{ctx: ctx, reg: reg, agent: o.Agent, engine: eng, session: session, now: now, st: router.LoadState(), preflt: map[string]string{}}
	ready, rows, skipped := scan.ready(cells)
	head := "rotation: role " + string(role)
	if s := scope.String(); s != "" {
		head += ", " + s
	}
	if len(ready) == 0 {
		o, err := noReadyCell(reg, o, session, scope)
		return ctx, o, roleNote{text: joinNotes(head+": no ready cell, the session route runs", skippedNote(skipped))}, err
	}
	cell, err := router.PickRoleCell(role, ready, !Peeking(ctx), now)
	if err != nil {
		head += " (pick unstamped: " + err.Error() + ")"
	}
	row := rows[cell.Key()]
	o.Model = cell.Model
	ctx, o = placeRotation(ctx, reg, o, row, scope)
	pick := fmt.Sprintf("%s -> %s %s on %s (%d of %d cells ready)", head, cell.Provider, cell.Model, row, len(ready), len(cells))
	return ctx, o, roleNote{text: joinNotes(pick, skippedNote(skipped)), account: router.AccountOf(reg.Row(row))}, nil
}

// noReadyCell keeps today's route, except under an exclusive mode: the start row alone, or a refusal with none.
func noReadyCell(reg *providers.Registry, o RunOptions, session string, scope modeScope) (RunOptions, error) {
	switch {
	case len(scope.off) == 0:
		return o, nil
	case reg.Row(session) == nil:
		return o, fmt.Errorf("rotation: exclusive %s in force, no cell is ready and no start row to stay on",
			strings.Join(scope.off, ", "))
	}
	o.Provider = session // the start is never an excluded provider, so no ladder hop crosses the boundary
	return o, nil
}

// untypedClaude reports a Claude dispatch nothing routes: no model, profile, lead, model pin, host or session.
func untypedClaude(ctx context.Context, o RunOptions) bool {
	named := []string{o.Model, o.Provider, o.Lead, o.Account, o.Host, o.SessionID}
	if !VaultAgentIsClaude(o.Agent) || slices.ContainsFunc(named, func(s string) bool { return s != "" }) {
		return false
	}
	if o.AutoModel || o.Direct || !o.Route.IsZero() || fenced(ctx, o) {
		return false
	}
	return agentModelPinContext(ctx, o.Agent) == "" // claude's record profile is its default lead, not a typed route
}

// fenced reports a run held to one route: an inherited arm or provider fence, a typed --fallback none or --as.
func fenced(ctx context.Context, o RunOptions) bool {
	typedNone := o.FallbackTyped && o.Fallback == router.FallbackNone // an env-default none is no request
	identity := o.Identity != "" && o.Identity != providers.IdentityAuto
	return confinedRoute(ctx) || typedNone || identity
}

// confinedRoute reports an inherited databricks:<p> arm or provider fence on ctx.
func confinedRoute(ctx context.Context) bool {
	confined, _ := router.ConfinedContext(ctx)
	return router.ArmProfileContext(ctx) != "" || confined != ""
}

// overlayRoutes reports an overlay that names where the dispatch runs; providerMode, effort and runner do not.
func overlayRoutes(ov agentconfig.Overlay) bool {
	identity := ov.Identity != "" && ov.Identity != providers.IdentityAuto
	return ov.Provider != "" || ov.Profile != "" || ov.Model != "" || ov.Account != "" || ov.Host != "" ||
		ov.Remote != "" || identity || ov.Fallback == router.FallbackNone
}

// dispatchRole is the role a brief's typed `Tier:` line names: deep is an owner, any other tier or none the fleet.
func dispatchRole(prompt string) router.Role {
	if m := tierLineRe().FindStringSubmatch(WithoutReviewLoop(prompt)); m != nil && strings.EqualFold(m[1], "deep") {
		return router.RoleOwner
	}
	return router.RoleFleet
}

// modeScope is how providerMode bounded a rotation: staying on an exclusive start, or exclusive providers kept out.
type modeScope struct {
	stay bool
	off  []string // exclusive provider names the dispatch did not start on
}

func (s modeScope) String() string {
	switch {
	case s.stay:
		return "stays on exclusive " + s.off[0]
	case len(s.off) > 0:
		return "exclusive " + strings.Join(s.off, ", ") + " kept out"
	}
	return ""
}

// modeCells is role's cells under providerMode: a start on an exclusive provider stays there, others never get it.
func modeCells(reg *providers.Registry, modes map[string]string, role router.Role, session string) ([]router.RoleCell, modeScope) {
	provs := router.ClaudeProviders(reg)
	cells := router.RoleCells(role, provs)
	exclusive := func(name string) bool {
		m, _ := router.ParseProviderMode(modes[name]) // validated when the overlay loaded
		return m == router.ModeExclusive
	}
	if start := router.RoleProviderOf(reg, session); start != "" && exclusive(start) {
		return slices.DeleteFunc(cells, func(c router.RoleCell) bool { return c.Provider != start }), modeScope{stay: true, off: []string{start}}
	}
	var scope modeScope
	for _, p := range provs {
		if exclusive(p.Name) {
			scope.off = append(scope.off, p.Name)
		}
	}
	return slices.DeleteFunc(cells, func(c router.RoleCell) bool { return exclusive(c.Provider) }), scope
}

// placeRotation leads o with row as a rotated lead minus kept-out rows; a pi sibling hop risk pins row explicitly.
func placeRotation(ctx context.Context, reg *providers.Registry, o RunOptions, row string, scope modeScope) (context.Context, RunOptions) {
	drop := scope.rows(reg)
	if scope.stay || slices.ContainsFunc(drop, func(r string) bool { return reg.Row(r).Preset == providers.PresetDatabricks }) {
		o.Provider = row
		return ctx, o
	}
	o.Lead = row
	return router.WithRotatedLead(ctx, router.RotatedLead{Row: row, Drop: drop}), o
}

// rows is the registry rows of the kept-out exclusive providers.
func (s modeScope) rows(reg *providers.Registry) []string {
	var out []string
	for _, name := range s.off {
		out = append(out, router.RoleRows(reg, name)...)
	}
	return out
}

// replayRotation re-leads a replayed rotation (--job, --retry, background): a recorded lead plus an untyped tier alias.
func replayRotation(ctx context.Context, o RunOptions) context.Context {
	// a recorded one-hop mode or identity replays on the recorded lead itself; only a confinement stops the re-lead
	replay := o.Lead != "" && !o.ModelTyped && slices.Contains(models.AliasNames, o.Model) && VaultAgentIsClaude(o.Agent)
	if !replay || confinedRoute(ctx) {
		return ctx
	}
	reg, err := providers.LoadShared()
	ov, oerr := agentconfig.LoadOverlayContext(ctx, "", nil) // kept-out rows come from today's overlay
	if err != nil || oerr != nil || overlayRoutes(ov) {
		return ctx
	}
	_, scope := modeCells(reg, ov.ProviderMode, router.RoleFleet, o.Lead)
	return router.WithRotatedLead(ctx, router.RotatedLead{Row: o.Lead, Drop: scope.keptOut(reg)})
}

// keptOut is the rows no hop may use: the kept-out providers', or under a stay every other Claude provider's.
func (s modeScope) keptOut(reg *providers.Registry) []string {
	if !s.stay {
		return s.rows(reg)
	}
	var others modeScope
	for _, p := range router.ClaudeProviders(reg) {
		if p.Name != s.off[0] {
			others.off = append(others.off, p.Name)
		}
	}
	return others.rows(reg)
}

// cellScan judges role cells for one dispatch: each cell's serving row (the session's first) or why none serves.
type cellScan struct {
	ctx     context.Context
	reg     *providers.Registry
	agent   string
	engine  Engine // the account gate's dispatch agent; nil skips that gate
	session string
	now     time.Time
	st      router.State
	preflt  map[string]string // a Databricks row's launch preflight refusal, checked once per row
}

// ready keeps the cells with a ready serving row, mapping each to it; the rest say why.
func (s cellScan) ready(cells []router.RoleCell) (ready []router.RoleCell, rows map[string]string, skipped []string) {
	rows = map[string]string{}
	for _, c := range cells {
		names := router.RoleRows(s.reg, c.Provider)
		if i := slices.Index(names, s.session); i > 0 {
			names = append([]string{s.session}, slices.Delete(slices.Clone(names), i, i+1)...)
		}
		var why []string
		for _, name := range names {
			reason := rowNotReady(s.reg.Row(name), s.st, s.agent, c.Model, s.now)
			if reason == "" { // the preflight probes only a row the cheap checks pass; it may clear a stale auth mark
				reason = cmp.Or(s.workspaceWhy(name), s.accountWhy(name, c.Model))
			}
			if reason != "" {
				why = append(why, name+" "+reason)
				continue
			}
			rows[c.Key()] = name
			break
		}
		if rows[c.Key()] == "" {
			skipped = append(skipped, c.Key()+": "+strings.Join(why, ", "))
			continue
		}
		ready = append(ready, c)
	}
	return ready, rows, skipped
}

// workspaceWhy is row's launch preflight refusal when it is a Databricks workspace: the explicit route's own check.
func (s cellScan) workspaceWhy(row string) string {
	p := s.reg.Row(row)
	if p == nil || p.Preset != providers.PresetDatabricks {
		return ""
	}
	if why, ok := s.preflt[row]; ok {
		return why
	}
	why := ""
	if resolved, err := agentconfig.CommandProvider(s.ctx, p); err != nil {
		why = "has an invalid tokenCommand overlay"
	} else if fs := launchWorkspaceFindings(s.ctx, s.agent, resolved, s.st, s.now, nil); len(fs) > 0 {
		why = "fails preflight: " + fs[0].Detail
	}
	s.preflt[row] = why
	return why
}

// rowNotReady is why p cannot run alias's Claude model for agent now: credential, login, tier, then rowHeld.
func rowNotReady(p *providers.Provider, st router.State, agentName, alias string, now time.Time) string {
	switch {
	case p == nil:
		return "is no registry row"
	case !providers.CredentialPresent(p):
		return "has no credential"
	case router.LoginDead(p):
		return "login is dead"
	case models.FamilyKind(p.Models[alias]) != models.FamilyClaude:
		return "maps no Claude " + alias
	}
	if ok, why := rowServes(agentName, p); !ok {
		return why
	}
	wire, err := spellModel(&ModelRoute{}, p, agentName, alias, now)
	if err != nil {
		return "cannot spell " + alias
	}
	if err := providers.ClaudeCodeError(p, wire); err != nil { // the route's own check: e.g. a Responses-only id
		return "cannot run " + wire + " in Claude Code"
	}
	return rowHeld(p, st, wire, now)
}

// rowHeld is why p's account or its wire model is out now: account or model cooling, a dead model, a listing not READY.
func rowHeld(p *providers.Provider, st router.State, wire string, now time.Time) string {
	acct := router.AccountOf(p)
	switch {
	case st.CoolingUntil(acct).After(now):
		return "is cooling"
	case st.ModelCoolingUntil(acct, wire).After(now) || st.ModelDead(acct, wire, now):
		return "holds " + wire + " cooling or dead"
	case p.Preset == providers.PresetDatabricks && router.ListedNotReady(p, wire, now):
		return "lists " + wire + " not READY"
	}
	return ""
}

// accountWhy is the launch's own account gate on row (exhausted, auth-marked, stalled), after the preflight renewed it.
func (s cellScan) accountWhy(row, alias string) string {
	p, eng := s.reg.Row(row), s.engine
	if p == nil || eng == nil {
		return ""
	}
	wire, err := spellModel(&ModelRoute{}, p, s.agent, alias, s.now)
	if err != nil {
		return ""
	}
	if e := accountRefused(Dispatch{Agent: eng, Account: p.Account()}, wire); e != nil {
		return "is refused: " + e.Err.Error()
	}
	return ""
}

func skippedNote(skipped []string) string {
	if len(skipped) == 0 {
		return ""
	}
	return "skipped " + strings.Join(skipped, "; ")
}

// rotationRows are the rows agent rotates on: every serving workspace under the pool pin, else its pinned row.
func rotationRows(reg *providers.Registry, agentName string) []*providers.Provider {
	if !rotatesUnderAuto(agentName) {
		return nil
	}
	profile := strings.TrimSpace(pinProfile(vault.GetAgentRecord(agentName)))
	if profile != PoolPin {
		if p := reg.Row(profile); router.RotationRow(p) {
			return []*providers.Provider{p}
		}
		return nil
	}
	var rows []*providers.Provider
	for _, n := range reg.ProfilesOf(providers.GroupDatabricks) {
		if p := reg.Providers[n]; CanServe(agentName, p) {
			rows = append(rows, p)
		}
	}
	return rows
}

// RotationModel is agent's next rotation model at alias across its rows; stamp records the pick, else it only peeks.
func RotationModel(reg *providers.Registry, agentName, alias string, stamp bool, now time.Time) (string, string, error) {
	return rotationModelOn(context.Background(), rotationRows(reg, agentName), agentName, alias, stamp, false, nil, now)
}

// rotationModelOn is RotationModel over the given rows.
func rotationModelOn(ctx context.Context, rows []*providers.Provider, agentName, alias string, stamp, tools bool, tried []string, now time.Time) (string, string, error) {
	if len(rows) == 0 {
		return "", "", nil
	}
	st := router.LoadState()
	var pool []string
	for _, p := range rows {
		for _, m := range withoutTried(rotationPool(agentName, alias, p, tools, now), tried, p) {
			// skips and holds live on the workspace's account, not the pool account the pick below reads
			held := router.HeldUntil(st[router.AccountOf(p)], agentName, m, now) != nil
			if !slices.Contains(pool, m) && (len(rows) == 1 || !held) {
				pool = append(pool, m)
			}
		}
	}
	account, p := router.AccountOf(rows[0]), rows[0]
	if len(rows) > 1 { // the pool pin: one history for every workspace, entries already wire ids
		account, p = router.ProviderAccount(PoolPin), nil
	}
	hist := account
	if rows[0].Preset == providers.PresetDatabricks {
		hist = router.ProviderAccount(PoolPin)
	}
	n := takeable(st[account], agentName, p, pool, now)
	dear := catalog.DeferTurns(pool, rotationTiers[alias])
	if !stamp {
		m := router.PeekRotation(agentName, account, hist, p, pool, dear, now)
		return m, rotationNote(m, alias, n, "next"), nil
	}
	m, err := router.PickRotation(ctx, agentName, account, hist, p, pool, dear, now)
	return m, rotationNote(m, alias, n, "picked"), err
}

func withoutTried(pool, tried []string, p *providers.Provider) []string {
	return slices.DeleteFunc(slices.Clone(pool), func(m string) bool {
		return slices.ContainsFunc(tried, func(t string) bool { return strings.TrimPrefix(t, p.Name+":") == strings.TrimPrefix(m, p.Name+":") })
	})
}

// takeable counts pool entries a pick could take: served and not on a capability skip.
func takeable(e router.Entry, agentName string, p *providers.Provider, pool []string, now time.Time) int {
	if p != nil {
		pool = router.ServedPool(p, pool, now)
	}
	return len(pool) - len(slices.DeleteFunc(slices.Clone(pool), func(m string) bool {
		return router.HeldUntil(e, agentName, m, now) == nil
	}))
}

// PoolMember is one rotation-pool model: the next pick, a live hold, and when it was last picked.
type PoolMember struct {
	Model      string     `json:"model"`
	Next       bool       `json:"next"`
	Held       bool       `json:"held"`
	HeldUntil  *time.Time `json:"held_until,omitempty"`
	LastPicked *time.Time `json:"last_picked,omitempty"`
}

// RotationMembers is the tier's pool on profile's row in pick order (least recent first, held last); read-only.
func RotationMembers(agentName, profile, tier string, now time.Time) []PoolMember {
	reg, err := providers.LoadShared()
	if err != nil {
		return nil
	}
	p := reg.Row(profile)
	if !router.RotationRow(p) {
		return nil
	}
	entries := router.ServedPool(p, rotationPool(agentName, cmp.Or(tierAliases[tier], models.Sonnet), p, false, now), now)
	st := router.LoadState()
	account, hist := router.AccountOf(p), router.AccountOf(p)
	if p.Preset == providers.PresetDatabricks {
		hist = router.ProviderAccount(PoolPin)
	}
	e, history := st[account], st[hist]
	var live []string
	var out []PoolMember
	for _, entry := range entries {
		m := PoolMember{Model: providers.SpellPin(p, entry)}
		key := router.DiscoverySkipKey(agentName, entry)
		if at := history.Discovery[key]; !at.IsZero() {
			m.LastPicked = &at
		}
		m.HeldUntil = router.HeldUntil(e, agentName, entry, now)
		m.Held = m.HeldUntil != nil
		if !m.Held {
			live = append(live, entry)
		}
		out = append(out, m)
	}
	next := router.PeekRotation(agentName, account, hist, p, live, catalog.DeferTurns(live, tier), now)
	for i := range out {
		out[i].Next = out[i].Model == next && next != ""
	}
	slices.SortStableFunc(out, func(a, b PoolMember) int {
		return cmp.Or(heldRank(a)-heldRank(b), timeOf(a.LastPicked).Compare(timeOf(b.LastPicked)))
	})
	return out
}

func heldRank(m PoolMember) int {
	if m.Held {
		return 1
	}
	return 0
}

func timeOf(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ErrPoolExhausted marks a fresh rotation pool with no eligible model.
var ErrPoolExhausted = fmt.Errorf("%w: auto pool exhausted", ErrModelRoute)

type autoStopError struct{ error }

func (e autoStopError) Unwrap() error { return e.error }

func freshRotation(p *providers.Provider, now time.Time) bool {
	if !router.RotationRow(p) {
		return false
	}
	if p.Preset == providers.PresetDatabricks {
		listing, _ := endpoints.Load(p.Name)
		return listing.Fresh(now)
	}
	return now.Sub(copilotauth.Time(p)) < serving.CacheTTL()
}

// rotationPick keeps a cold listing on the tier fallback and refuses an exhausted fresh pool.
func rotationPick(reg *providers.Registry, q ModelQuery, base ModelRoute, need autoNeed, c *AutoChoice, now time.Time) (ModelRoute, bool, error) {
	p := reg.Row(base.Profile)
	if !freshRotation(p, now) {
		return base, false, nil
	}
	if !rotatesUnderAuto(q.Agent) {
		return autoFallback(reg, q, base, need, c, q.Agent+" keeps its tier model under auto", now), true, nil
	}
	m, note, alias, err := tierRotation(q, cmp.Or(tierAliases[need.tier], models.Sonnet), p, now)
	if err != nil {
		return base, true, autoStopError{fmt.Errorf("auto pick state: %w", err)}
	}
	if m == "" {
		return base, true, exhaustedPool(q.Agent, alias, p, q.Tools, now)
	}
	pq := q
	pq.Route, pq.Model, pq.Profile, pq.Default = rowRoute(reg, base.Profile), m, "", ""
	r, err := ResolveModel(reg, pq, now)
	if err != nil {
		return base, true, err
	}
	r.Source, c.Model, c.Criterion = base.Source, r.Model, "rotation"
	r.Note = joinNotes(routeNote(base.Note), r.Note, note)
	r.Effort = autoEffort(r.Effort, q.Agent, need)
	return r, true, nil
}

// tierRotation is the pick at alias on p, stepping to standard when the row serves nothing in alias's tier.
func tierRotation(q ModelQuery, alias string, p *providers.Provider, now time.Time) (string, string, string, error) {
	ctx := cmp.Or(q.ctx, context.Background())
	stamp := !Peeking(ctx)
	m, note, err := rotationModelOn(ctx, []*providers.Provider{p}, q.Agent, alias, stamp, q.Tools, askTriedPicks(q.ctx), now)
	if err == nil && m == "" && alias != models.Sonnet && len(heldIn(q.Agent, alias, p, q.Tools, now)) == 0 {
		alias = models.Sonnet // the row serves nothing in this tier: step to standard, as the tier ladder does
		m, note, err = rotationModelOn(ctx, []*providers.Provider{p}, q.Agent, alias, stamp, q.Tools, askTriedPicks(q.ctx), now)
	}
	return m, note, alias, err
}

func exhaustedPool(agentName, alias string, p *providers.Provider, tools bool, now time.Time) error {
	held := heldIn(agentName, alias, p, tools, now)
	return fmt.Errorf("%w on %s; held ids: %s", ErrPoolExhausted, p.Name, cmp.Or(strings.Join(held, ", "), "none"))
}

// heldIn is the pool ids of alias's tier on p that a skip or hold keeps out of the pick.
func heldIn(agentName, alias string, p *providers.Provider, tools bool, now time.Time) []string {
	e := router.LoadState()[router.AccountOf(p)]
	var held []string
	for _, id := range rotationPool(agentName, alias, p, tools, now) {
		if router.HeldUntil(e, agentName, id, now) != nil {
			held = append(held, id)
		}
	}
	return held
}

// rotationNote names the pick, its tier and pool size, plus R5's review rule for a cheap or new-family author.
func rotationNote(m, alias string, n int, verb string) string {
	if m == "" {
		return ""
	}
	note := fmt.Sprintf("rotation %s %s (%s tier, %d in pool)", verb, m, rotationTiers[alias], n)
	rk, fam := catalog.RankOf(m), models.FamilyKind(m)
	if !rk.Known || rk.Capability < 5 || fam != models.FamilyClaude && fam != models.FamilyGPT {
		note += "; R5: full read and a capability-5 other-family review"
	}
	return note
}

// RotationFreeze freezes the launch's tool-aware rotation pick: its serving profile, plain model id and note.
func RotationFreeze(ctx context.Context, agentName, brief, model string) (profile, id, note string) {
	ctx, _ = RunValues(ctx, nil)
	o, err := agentconfig.LoadOverlayContext(ctx, "", nil)
	if err != nil || o.Host != "" || o.Remote != "" {
		return "", "", ""
	}
	model = cmp.Or(model, o.Model)
	if !IsAuto(cmp.Or(model, agentModelPinContext(ctx, agentName), AutoModel)) {
		return "", "", ""
	}
	reg, err := providers.LoadShared()
	if err != nil {
		return "", "", ""
	}
	q := ModelQuery{Agent: agentName, Model: model, Task: brief, Tools: IsReadOnlyBrief(brief) && agentName != agentid.Codex,
		Profile: o.Profile, Provider: o.Provider, Account: o.Account, Effort: o.Effort, Identity: o.Identity, Fallback: o.Fallback}
	q.Default, _ = TaskTier(brief)
	return freezeQuery(ctx, reg, q, agentName, brief)
}

// freezeQuery resolves q as the launch would and returns the rotation pick's profile, model id and note.
func freezeQuery(ctx context.Context, reg *providers.Registry, q ModelQuery, agentName, brief string) (string, string, string) {
	q, note, err := RunQuery(ctx, reg, q, router.RuleClassifier{}.Alias(brief))
	if err != nil || q.Agent != agentName {
		return "", "", "rotation route unavailable; the run picks at launch"
	}
	r, choice, err := ExplainRun(reg, QueryFor(ctx, q), time.Now())
	if err != nil {
		return "", "", "rotation state unavailable; the run picks at launch"
	}
	if choice == nil || choice.Criterion != "rotation" || r.Profile == "" {
		return "", "", ""
	}
	return r.Profile, r.Model, joinNotes(note, r.Note)
}

// RotationSummary is a rotation pin's doctor detail: the pool size per tier, then the last 7 days' runs by model.
func RotationSummary(agentName string, now time.Time) string {
	reg, err := providers.LoadShared()
	if err != nil {
		return "rotating; registry unreadable"
	}
	rows := rotationRows(reg, agentName)
	var sizes []string
	for _, alias := range []string{models.Haiku, models.Sonnet, models.Opus} {
		n := 0
		for _, p := range rows {
			n = max(n, len(rotationPool(agentName, alias, p, false, now)))
		}
		sizes = append(sizes, fmt.Sprintf("%s %d", rotationTiers[alias], n))
	}
	return "rotating " + strings.Join(sizes, ", ") + "; 7d " + recentPicks(agentName, now)
}

// recentPicks is agent's last 7 days of runs counted by model, top four: "kimi-k3 3, gpt-6-2-sol 2".
func recentPicks(agentName string, now time.Time) string {
	counts := map[string]int{}
	routes := map[string]int{}
	_ = eachRecent(paths.DispatchDir(), jsonToken(agentName), func(r Record) bool { // an unreadable dir lists none
		m := cmp.Or(r.Model, r.Invoked)
		if r.Agent != agentName || now.Sub(r.Time) >= 7*24*time.Hour {
			return true
		}
		if !r.RanRoute.IsZero() {
			routes[strings.TrimPrefix(route.Join(r.RanRoute.Provider, r.RanRoute.Profile), "databricks:")]++
		}
		if m == "" {
			return true
		}
		if _, after, ok := strings.Cut(m, ":"); ok {
			m = after
		}
		if m == AutoModel { // a run that never named its model is no pick
			return true
		}
		counts[catalog.ID(m)]++
		return true
	})
	keys := slices.SortedFunc(maps.Keys(counts), func(a, b string) int { return cmp.Or(counts[b]-counts[a], strings.Compare(a, b)) })
	var out []string
	for _, k := range keys[:min(len(keys), 4)] {
		out = append(out, fmt.Sprintf("%s %d", k, counts[k]))
	}
	summary := cmp.Or(strings.Join(out, ", "), "no runs")
	var routeCounts []string
	for _, route := range slices.Sorted(maps.Keys(routes)) {
		routeCounts = append(routeCounts, fmt.Sprintf("%s %d", route, routes[route]))
	}
	if len(routeCounts) > 0 {
		summary += "; routes " + strings.Join(routeCounts, ", ")
	}
	return summary
}

// slowHold is how long a pool model that hit the deadline or went silent mid-task stays out of auto picks.
const slowHold = 15 * time.Minute

func progressed(err error) bool {
	e, ok := errors.AsType[*retry.Error](err)
	return ok && e.Progressed
}

// noteSlowModel holds a pool model out of auto picks after a deadline or idle cutoff; the account stays usable.
func noteSlowModel(disp Dispatch, req RunOptions, model string, err error) {
	k := CutoffOf(err)
	if (k != CutoffDeadline && k != CutoffIdle) || model == "" || disp.Account == "" {
		return
	}
	if k == CutoffDeadline && (req.ModelTyped || req.TimeoutTyped || progressed(err)) {
		return // a typed model or deadline, or a run that streamed, says nothing about the model's health
	}
	_, row, _ := strings.Cut(disp.Account, "·")
	p := loadRegistryWarn().Row(row)
	if !router.IsPoolModel(poolOf(engineName(disp), p), p, model) {
		return
	}
	now := time.Now()
	warnHold(disp.Account, markModelDead(disp.Account, model, now.Add(slowHold), now))
}
