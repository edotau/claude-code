package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/vault"
)

// RunQuery is q as the run's first hop resolves it; a routed external dispatch takes the ladder's first hop.
func RunQuery(ctx context.Context, reg *providers.Registry, q ModelQuery, aliasHint string) (ModelQuery, string, error) {
	return runQueryRoute(ctx, reg, q, aliasHint)
}

func runQueryRoute(ctx context.Context, reg *providers.Registry, q ModelQuery, aliasHint string) (ModelQuery, string, error) {
	if err := refuseOwnAuto(q.Agent, q.Model); err != nil {
		return q, "", err
	}
	if ownConnectorQuery(q) {
		return q, "", nil
	}
	if hop, ok := agyAliasQuery(q); ok {
		return hop, "", nil
	}
	if pq, ok, err := poolQuery(ctx, reg, q); ok {
		return pq, "", err
	}
	if namesRoute(q) {
		return q, "", nil
	}
	pin := agentModelPinContext(ctx, q.Agent)
	chain, _, err := Resolve(PeekContext(ctx), RouteReq{Agent: q.Agent, AliasHint: aliasHint, Fallback: q.Fallback, Pinned: true})
	if err != nil {
		return q, "", err
	}
	if len(chain.Dispatches) == 0 {
		return q, "", fmt.Errorf("no route runs %s (%s)", q.Agent, strings.Join(chain.Skipped, "; "))
	}
	return labelHop(reg, q, pin, chain.Dispatches[0])
}

// NextLabel is the label of an auto the run picks itself: the peeked id, said to be only the next pick.
func NextLabel(id string) string { return "auto (next: " + id + ")" }

// IsNextLabel reports a NextLabel-shaped head.
func IsNextLabel(head string) bool {
	return strings.HasPrefix(head, "auto (next:") && strings.HasSuffix(head, ")")
}

// AutoRun reports a query whose run picks its own model: a typed or pinned auto, not a concrete model.
func AutoRun(ctx context.Context, q ModelQuery) bool {
	_, auto := SplitAuto(cmp.Or(q.Model, agentModelPinContext(ctx, q.Agent)))
	return auto
}

// namesRoute reports a query that runs as typed: claude, a typed model, or a named profile, provider or account.
func namesRoute(q ModelQuery) bool {
	prefix, auto := SplitAuto(q.Model)
	typed := q.Model != "" && (!auto || prefix != "") // a bare auto routes like an untyped run
	return VaultAgentIsClaude(q.Agent) || typed || q.Profile != "" || q.Provider != "" || q.Account != ""
}

// PoolPicks reports a run whose pool pin takes the least-busy workspace: no provider prefix, not live or detached.
func PoolPicks(ag, model string, held bool) bool {
	return !held && !strings.Contains(model, ":") && PinsPool(ag)
}

// poolQuery fixes a stored pool pin to the session workspace, else the first signed-in workspace in registry order.
func poolQuery(ctx context.Context, reg *providers.Registry, q ModelQuery) (ModelQuery, bool, error) {
	if !q.Route.IsZero() || namesRoute(q) || !PinsPool(q.Agent) {
		return q, false, nil
	}
	profile := storedPoolProfile(ctx, reg, q.Session)
	if profile == "" {
		return q, true, fmt.Errorf("%w: no signed-in Databricks workspace for %s", ErrModelRoute, q.Agent)
	}
	q.Route = rowRoute(reg, profile)
	q.Provider, q.Profile = q.Route.Provider, q.Route.Profile
	return q, true, nil
}

func storedPoolProfile(ctx context.Context, reg *providers.Registry, session string) string {
	if p := reg.Row(cmp.Or(session, providers.SessionProfileContext(ctx))); p != nil && p.Preset == providers.PresetDatabricks {
		return p.Name
	}
	for _, name := range reg.ProfilesOf(providers.GroupDatabricks) {
		if p := reg.Row(name); p.Preset == providers.PresetDatabricks && providers.CredentialPresent(p) {
			return name
		}
	}
	return ""
}

// labelHop is ladderHop under auto: a typed or pinned auto beats the hop's cell model, as dispatchRequest runs it.
func labelHop(reg *providers.Registry, q ModelQuery, pin string, d Dispatch) (ModelQuery, string, error) {
	_, auto := SplitAuto(q.Model)
	hop, note, err := ladderHop(reg, q, d)
	if _, pinAuto := SplitAuto(pin); auto || pinAuto {
		hop.Model = AutoCell(hop.Model)
	}
	return hop, note, err
}

// AutoCell is auto on a ladder cell's row: `<row>:auto` for a `<row>:<model>` cell, else the bare auto.
func AutoCell(cell string) string {
	if row, _, ok := strings.Cut(cell, ":"); ok && row != "" {
		return row + ":" + AutoModel
	}
	return AutoModel
}

// rowRoute is the whole route of registry row: its own provider and its name as the profile.
func rowRoute(reg *providers.Registry, row string) route.Route {
	r := route.Route{Profile: row}
	if p := reg.Row(row); p != nil {
		r.Provider = p.Provider
	}
	return r
}

// ladderHop is the query d's engine resolves: a `<row>:<model>` cell as typed, an alias cell the row's pin or tier.
func ladderHop(reg *providers.Registry, q ModelQuery, d Dispatch) (ModelQuery, string, error) {
	hop := q
	hop.Agent = d.Agent.Name()
	acct := d.Account
	if _, row, ok := strings.Cut(acct, "·"); ok {
		acct = row
	}
	if reg.Row(acct) != nil {
		hop.Profile = acct
	}
	if !d.keepCaller && !d.unroutedDispatch && (strings.Contains(d.Model, ":") || VaultAgentIsClaude(hop.Agent)) {
		hop.Model = d.Model // dispatchRequest's CellModel: a bare alias is dropped for a vendor CLI's own pin
	}
	note := ""
	if hop.Agent != q.Agent {
		note = fmt.Sprintf("%s is gated out; the route runs %s", q.Agent, hop.Agent)
	}
	return hop, note, nil
}

// ResolveRun is ResolveModel with auto: a typed or pinned auto, or nothing named, ranks the row's served models.
func ResolveRun(reg *providers.Registry, q ModelQuery, now time.Time) (ModelRoute, error) {
	r, _, err := ExplainRun(reg, q, now)
	return r, err
}

// ExplainRun is ResolveRun plus its auto choice; an unlisted Databricks model is marked; none refuses it unless auto.
func ExplainRun(reg *providers.Registry, q ModelQuery, now time.Time) (ModelRoute, *AutoChoice, error) {
	if err := refuseOwnAuto(q.Agent, q.Model); err != nil {
		return ModelRoute{}, nil, err
	}
	if ownConnectorQuery(q) {
		return ownConnectorRoute(q), nil, nil
	}
	q, need := autoQuery(q)
	if need.mode != autoOff {
		menu := providers.OpencodeMenu()
		q.menu = &menu
	}
	r, err := autoBase(reg, q, need, now)
	if err != nil && need.mode != autoOff && errors.Is(err, ErrModelRoute) {
		need.noTier = true // no tier model runs here, so the agent's default stands in
		r, err = rowDefault(reg, q, need, cmp.Or(need.prefix, q.Profile), now)
	}
	if err != nil {
		return r, nil, err
	}
	r, choice, err := autoPick(reg, q, r, need, now)
	if err != nil {
		return r, choice, err
	}
	r, err = checkRouteServed(reg, r, q.Fallback == router.FallbackNone && need.mode == autoOff, now)
	return r, choice, err
}

// rowDefault is an untyped run's model on profile: the concrete pin, else the row's opus, by id if refused as an alias.
func rowDefault(reg *providers.Registry, q ModelQuery, need autoNeed, profile string, now time.Time) (ModelRoute, error) {
	q.Model, q.Profile, q.Default, q.PinModel, q.Route = "", "", "", need.pin, rowRoute(reg, profile)
	r, err := ResolveModel(reg, q, now)
	p := reg.Row(profile)
	if err == nil || p == nil || !errors.Is(err, ErrModelRoute) {
		return r, err
	}
	wire, serr := spellModel(&ModelRoute{}, p, q.Agent, models.Opus, now)
	if serr != nil {
		return r, err
	}
	q.Model, q.PinModel, q.rowSpelled = wire, "", true
	return ResolveModel(reg, q, now)
}

// resolveTier is ResolveModel with q.Default as the task tier; a tier the row cannot serve keeps the untiered pick.
func resolveTier(reg *providers.Registry, q ModelQuery, now time.Time) (ModelRoute, error) {
	r, err := ResolveModel(reg, q, now)
	if q.Default == "" || q.Model != "" || err == nil && (q.PinModel != "" || tierServed(reg, r, now)) {
		return r, err
	}
	tier := q.Default
	q.Default = ""
	base, berr := ResolveModel(reg, q, now)
	if berr != nil {
		return r, err
	}
	why := fmt.Sprintf("task tier %s is not served on %s, so running %s", tier, base.Profile, base.Model)
	base.Note = joinNotes(why, base.Note)
	return base, nil
}

// autoBase is resolveTier; under auto a tier alias the row cannot run steps to another, strongest first.
func autoBase(reg *providers.Registry, q ModelQuery, need autoNeed, now time.Time) (ModelRoute, error) {
	base, err := resolveTier(reg, q, now)
	if err == nil || need.mode == autoOff || !errors.Is(err, ErrModelRoute) {
		return base, err
	}
	for _, alias := range []string{models.Opus, models.Sonnet, models.Haiku} {
		q.Default = alias
		if need.prefix != "" {
			q.Model = need.prefix + ":" + alias
		}
		if r, rerr := resolveTier(reg, q, now); rerr == nil {
			return r, nil
		}
	}
	return base, err
}

func joinNotes(notes ...string) string {
	return strings.Join(slices.DeleteFunc(notes, func(s string) bool { return s == "" }), "; ")
}

// tierServed is false only when r's Databricks row has a fresh listing without r's model READY.
func tierServed(reg *providers.Registry, r ModelRoute, now time.Time) bool {
	p := reg.Row(r.Profile)
	if p == nil || p.Preset != providers.PresetDatabricks {
		return true
	}
	return !router.ListedNotReady(p, r.Model, now)
}

// tierAliases maps a brief's `Tier:` word to the alias each row spells as its model of that tier.
var tierAliases = map[string]string{"fast": models.Haiku, "standard": models.Sonnet, "deep": models.Opus}

var (
	tierLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?mi)^[ \t]*Tier:[ \t]*(fast|standard|deep)\b`) })
	// deepTaskRe: costly-mistake or large-judgment work (audit, security, auth, parsers, design, migrations, secrets).
	deepTaskRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(audit\w*|security|safety|threat\w*|parser\w*|parsing|grammar|judg(?:e)?ment|` +
			`planning|design|redesign|architect\w*|permissions?|auth[nz]?|authenticat\w*|authoriz\w*|migrat\w*|` +
			`irreversible|credentials?|secrets?)\b`)
	})
	fastTaskRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(typos?|renames?|one[- ]line(?:r)?|reword|wording|commit message|message[- ]only|moves?)\b`)
	})
	scopeTaskRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(across|every|all|repo-wide|codebase|call ?sites|callers|everywhere|throughout)\b`)
	})
	fileTaskRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`[\w.-]*\w\.(?:go|py|md|sh|js|ts|tsx|json|ya?ml|toml|sql|rs|java|rb|css|html|tmpl)\b`)
	})
	// boilerRe: a brief's header lines and code spans name commands and paths, not the work.
	boilerRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("(?m)^[ \t]*(?:Run|Owned|Mode|Verify|Report|Tier):.*$|`[^`\n]*`")
	})
)

// TaskTier is the model alias a brief's work needs: its `Tier:` line, else deep, fast or standard; "" for no text.
func TaskTier(brief string) (alias, signal string) {
	if m := tierLineRe().FindStringSubmatch(brief); m != nil {
		return tierAliases[strings.ToLower(m[1])], "Tier: " + strings.ToLower(m[1])
	}
	text, _, _ := strings.Cut(brief, "\n## Leaf rules") // agents brief's footer is the same on every leaf
	text = strings.TrimSpace(boilerRe().ReplaceAllString(text, ""))
	if text == "" {
		return "", ""
	}
	if words := deepTaskRe().FindAllString(text, -1); len(words) > 0 {
		return models.Opus, "deep:" + deepSignal(words)
	}
	if rule := fastTask(text); rule != "" {
		return models.Haiku, rule
	}
	return models.Sonnet, "standard"
}

// deepSignal is the deep word that names the cost of a mistake (safety, auth), else the first one.
func deepSignal(words []string) string {
	for _, w := range words {
		if !complexityRe().MatchString(strings.ToLower(w)) {
			return strings.ToLower(w)
		}
	}
	return strings.ToLower(words[0])
}

// fastTask names why text is a short question or one mechanical edit on at most two files; "" when it is not.
func fastTask(text string) string {
	switch tier, rule := router.EffortTierSignal(text); {
	case tier == models.EffortLow:
		return rule
	case tier == models.EffortHigh && rule != "paths>=2": // high-effort work stays off fast; the file cap is below
		return ""
	}
	if len(text) >= 1000 || !fastTaskRe().MatchString(text) || scopeTaskRe().MatchString(text) {
		return ""
	}
	files := map[string]bool{}
	for _, f := range fileTaskRe().FindAllString(text, -1) {
		files[f] = true
	}
	if len(files) <= 2 {
		return "mechanical"
	}
	return ""
}

// AutoModel is the model value that has the resolver pick per task: typed, in an overlay, <row>:auto or a record pin.
const AutoModel = router.AutoArm

// SplitAuto reports whether model asks for auto ("auto" or "<prefix>:auto"); prefix is "" for the bare form.
func SplitAuto(model string) (prefix string, ok bool) {
	m := strings.TrimSpace(model)
	if strings.EqualFold(m, AutoModel) {
		return "", true
	}
	prefix, rest, cut := strings.Cut(m, ":")
	return prefix, cut && prefix != "" && strings.EqualFold(rest, AutoModel)
}

type autoMode int

const (
	autoOff      autoMode = iota // a typed or pinned concrete model runs as named
	autoImplicit                 // nothing names a model: rank, say only a step down
	autoAsked                    // auto typed or pinned: rank and say why
)

// autoNeed is what the brief asks of the model: its tier, the signal that sized it, and how auto was reached.
type autoNeed struct {
	tier, signal, prefix string // prefix: the <row> of a typed <row>:auto
	pin                  string // the record's concrete pin, which a run with no --model takes
	mode                 autoMode
	noTier               bool // no tier alias's model runs on the row: the base is rowDefault
	unlisted             bool // the row has no fresh listing to rank
}

// autoQuery sizes q's tier and folds auto out of it: the auto word becomes the tier alias on its row, the pin drops.
func autoQuery(q ModelQuery) (ModelQuery, autoNeed) {
	alias, signal := briefTier(q)
	q.Default = alias
	need := autoNeed{tier: tierOf(alias), signal: signal}
	prefix, typed := SplitAuto(q.Model)
	_, pinned := SplitAuto(q.PinModel)
	need.prefix = prefix
	if !pinned {
		need.pin = q.PinModel
	}
	switch {
	case typed || q.Model == "" && pinned:
		need.mode = autoAsked
	case q.Model == "" && q.PinModel == "" && alias != "":
		need.mode = autoImplicit
	default:
		return q, need
	}
	if need.tier == "" {
		q.Default = untieredAuto(q.Agent)
		need.tier, need.signal = tierOf(q.Default), "no brief"
	}
	q.Model, q.PinModel = "", ""
	if prefix != "" {
		q.Model = prefix + ":" + q.Default
	}
	return q, need
}

// briefTier is the task's tier alias and signal, else q's default tier.
func briefTier(q ModelQuery) (string, string) {
	if q.Task == "" {
		return q.Default, "tier " + q.Default
	}
	return TaskTier(q.Task)
}

// tierOf is the tier word whose alias is alias; "" for none.
func tierOf(alias string) string {
	for tier, a := range tierAliases {
		if a == alias {
			return tier
		}
	}
	return ""
}

// TierEffort is the reasoning effort a task-tier alias runs at; "" for no tier.
func TierEffort(alias string) string { return tierEfforts[alias] }

var tierEfforts = map[string]string{models.Haiku: models.EffortLow, models.Sonnet: models.EffortMedium, models.Opus: models.EffortHigh}

// AutoChoice is why auto ran its model: the tier, the deciding criterion and signal, and the ranked candidates.
type AutoChoice struct {
	Tier       string           `json:"tier"`
	Criterion  string           `json:"criterion"`
	Signal     string           `json:"signal"`
	Model      string           `json:"model"`
	Candidates int              `json:"candidates"` // served models the agent can run on the row
	Stepped    bool             `json:"stepped,omitempty"`
	FitShadow  bool             `json:"fit_shadow,omitempty"` // the ranked fit scores are shown, not applied
	Fallback   string           `json:"fallback,omitempty"`   // why no ranking decided
	Ranked     []catalog.Ranked `json:"ranked"`               // eligible candidates in pick order
}

// Reason is the one-line why, e.g. `auto: deep (accuracy: safety) -> M; 3 candidates at or above`.
func (c AutoChoice) Reason() string {
	head := fmt.Sprintf("auto: %s (%s: %s) -> %s", c.Tier, c.Criterion, c.Signal, c.Model)
	switch {
	case c.Fallback != "":
		return head + "; " + c.Fallback
	case c.Criterion == "rotation":
		return head + "; least-recently-picked of the tier's rotation pool"
	case c.Stepped:
		return fmt.Sprintf("%s; none of %d at or above %s, stepped down to the strongest served", head, c.Candidates, c.Tier) + c.fitNote()
	}
	return fmt.Sprintf("%s; %d %s at or above", head, len(c.Ranked), textutil.PluralWord(len(c.Ranked), "candidate", "candidates")) + c.fitNote()
}

// fitNote is the picked candidate's learned fit, "; fit 0.85 avg 42s (shadow)", or "" when it has none.
func (c AutoChoice) fitNote() string {
	if len(c.Ranked) == 0 || c.Ranked[0].FitNote() == "" {
		return ""
	}
	return "; " + c.Ranked[0].FitNote()
}

// complexityRe: deep signals that are about the size of the judgment, not the cost of a mistake.
var complexityRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(audit\w*|planning|design|redesign|architect\w*|judg(?:e)?ment)$`)
})

var tierCriteria = map[string]string{"fast": "speed", "standard": "value", "deep": "accuracy"}

// newAutoChoice names the criterion that decides need: speed for fast, value for standard, accuracy or complexity.
func newAutoChoice(need autoNeed) *AutoChoice {
	signal, deep := strings.CutPrefix(need.signal, "deep:")
	crit := tierCriteria[need.tier]
	if deep && complexityRe().MatchString(signal) {
		crit = "complexity"
	}
	return &AutoChoice{Tier: need.tier, Criterion: crit, Signal: signal}
}

// autoPick ranks the served models of base's row for need and resolves the winner; no ranking keeps base.
func autoPick(reg *providers.Registry, q ModelQuery, base ModelRoute, need autoNeed, now time.Time) (ModelRoute, *AutoChoice, error) {
	if need.mode == autoOff {
		return base, nil, nil
	}
	c := newAutoChoice(need)
	if need.mode == autoAsked {
		if r, ok, err := rotationPick(reg, q, base, need, c, now); ok {
			return r, c, err
		}
	}
	cands, why := autoCandidates(reg, q, reg.Row(base.Profile), now)
	c.Candidates = len(cands)
	ch := catalog.Choose(cands, q.Agent, need.tier, q.Tools)
	c.Ranked, c.Stepped, c.FitShadow = ch.Ranked, ch.Stepped, ch.Shadow
	if len(c.Ranked) == 0 {
		if why == "" && need.mode == autoAsked && !need.noTier { // a fresh listing, nothing to rank: run the untyped pick
			if d, err := rowDefault(reg, q, need, base.Profile, now); err == nil {
				base, need.noTier = d, true
			}
		}
		need.unlisted = why != ""
		return autoFallback(reg, q, base, need, c, cmp.Or(why, "no served model fits"), now), c, nil
	}
	pq := q
	pq.Route, pq.Model, pq.Profile, pq.Default = rowRoute(reg, base.Profile), c.Ranked[0].Model, "", ""
	r, err := ResolveModel(reg, pq, now)
	if err != nil {
		return autoFallback(reg, q, base, need, c, strings.TrimPrefix(err.Error(), ErrModelRoute.Error()+": "), now), c, nil
	}
	r.Source, c.Model = base.Source, r.Model
	r.Note = joinNotes(routeNote(base.Note), c.note(need.mode))
	r.Effort = autoEffort(r.Effort, q.Agent, need)
	return r, c, nil
}

// note is the reason a route carries: always for an asked auto, for an implicit one only when it stepped down.
func (c AutoChoice) note(mode autoMode) string {
	if mode == autoAsked || c.Stepped {
		return c.Reason()
	}
	return ""
}

// routeNote is base's note without resolveTier's tier fallback, which a ranked pick replaces.
func routeNote(note string) string {
	parts := slices.DeleteFunc(strings.Split(note, "; "), func(s string) bool { return strings.HasPrefix(s, "task tier ") })
	return joinNotes(parts...)
}

// autoFallback keeps base, the tier alias's model or rowDefault, when nothing ranked; a model named auto never runs.
func autoFallback(reg *providers.Registry, q ModelQuery, base ModelRoute, need autoNeed, c *AutoChoice, why string,
	now time.Time) ModelRoute {
	alias := cmp.Or(base.Alias, tierAliases[need.tier])
	c.Fallback = fmt.Sprintf("%s, ran %s", why, coldRanModel(reg.Row(base.Profile), base.Model, alias))
	if need.noTier {
		c.Fallback = fmt.Sprintf("%s, ran %s's default model on %s", why, q.Agent, base.Profile)
	}
	if base.Model == AutoModel {
		base, c.Fallback = registryAuto(reg, q, base, alias, c.Fallback, now)
	}
	c.Model = base.Model
	if p := reg.Row(base.Profile); need.mode == autoAsked || need.unlisted && p != nil && p.Preset == providers.PresetDatabricks {
		base.Note = joinNotes(base.Note, c.Reason()) // without a listing an implicit run takes the tier model: say so
	}
	base.Effort = autoEffort(base.Effort, q.Agent, need)
	return base
}

// coldRanModel names what a cold fallback runs: a github row's own model id, else the row's alias model.
func coldRanModel(p *providers.Provider, model, alias string) string {
	if p != nil && p.Provider == "github" && model != "" && model != AutoModel {
		return model
	}
	return "the row's " + alias + " model"
}

// registryAuto swaps a row's model named auto (its provider's own router) for a stand-in the row runs.
func registryAuto(reg *providers.Registry, q ModelQuery, base ModelRoute, alias, why string, now time.Time) (ModelRoute, string) {
	why += "; its model auto is " + base.Profile + "'s own router, not a pick"
	for _, in := range autoStandIns(q.Agent, alias, reg.Row(base.Profile)) {
		q.Route, q.Model, q.Profile, q.Default = rowRoute(reg, base.Profile), in.model, "", ""
		if r, err := ResolveModel(reg, q, now); err == nil && r.Model != AutoModel {
			r.Source = base.Source
			return r, why + "; ran " + in.from
		}
	}
	return base, why
}

type standIn struct{ model, from string }

// autoStandIns are the route table's model for agent at alias, then the row's opencode_fallback.
func autoStandIns(agentName, alias string, p *providers.Provider) []standIn {
	var out []standIn
	if tb, err := router.Load(); err == nil {
		if c, ok := tb.Cell(agentName, alias); ok && c.Model != "" {
			out = append(out, standIn{c.Model, "the route table's " + agentName + " " + alias + " model"})
		}
	}
	if p != nil && p.OpencodeFallback != "" {
		out = append(out, standIn{p.OpencodeFallback, "its opencode_fallback"})
	}
	return out
}

// autoEffort is the tier's effort for an asked auto run unless the query or the agent's record names one.
func autoEffort(cur, agentName string, need autoNeed) string {
	if cur != "" || need.mode != autoAsked || vault.GetAgentRecord(agentName).Effort != "" {
		return cur
	}
	return TierEffort(tierAliases[need.tier])
}

// autoCandidates is every READY chat model on p's fresh listing the agent can run there, with its tool probe.
func autoCandidates(reg *providers.Registry, q ModelQuery, p *providers.Provider, now time.Time) ([]catalog.Candidate, string) {
	if p == nil {
		return nil, "no row"
	}
	s, ok := router.FreshServed(p.Name, now)
	if p.Preset != providers.PresetDatabricks || !ok {
		return nil, "no fresh model listing on " + p.Name
	}
	var probes map[string]serving.CheckResult // only a tools run reads Tools
	if q.Tools {
		probes, _ = catalog.LoadCheckCache(p.Name)
	}
	var out []catalog.Candidate
	seen := map[string]bool{}
	for _, m := range s.Models {
		wire, ok := servedCandidate(reg, q, p, m, now)
		if ok && !seen[wire] {
			seen[wire] = true
			out = append(out, catalog.Candidate{Model: wire, Tools: probes[catalog.ID(wire)].Tools})
		}
	}
	return out, ""
}

// servedCandidate is m's wire id when m is a READY chat endpoint the agent runs on p, by the launch's own checks.
func servedCandidate(reg *providers.Registry, q ModelQuery, p *providers.Provider, m router.ServedModel, now time.Time) (string, bool) {
	if m.Task != chatTask || !m.IsReady() || catalog.ID(m.ID) == AutoModel {
		return "", false
	}
	r, err := ResolveModel(reg, ModelQuery{Agent: q.Agent, Route: rowRoute(reg, p.Name), Model: m.ID, menu: q.menu}, now)
	return r.Model, err == nil && agentRunsIn(q.menuIn(), q.Agent, p, r.Model)
}

// chatTask is the serving task of a chat-completions endpoint.
const chatTask = "llm/v1/chat"

// agentRuns is the tier rule: claude runs Claude, gemini Gemini (any chat model on Databricks), codex Responses.
func agentRuns(agentName string, p *providers.Provider, wire string) bool {
	return agentRunsIn(nil, agentName, p, wire)
}

func agentRunsIn(menu []providers.OpencodeModel, agentName string, p *providers.Provider, wire string) bool {
	switch facts := agentid.Routing(agentName); {
	case facts.ModelFamily == string(models.FamilyClaude):
		return models.FamilyKind(wire) == models.FamilyClaude
	case facts.GoogleOrDatabricks:
		return models.FamilyKind(wire) == models.FamilyGemini || GeminiGatewayRow(p)
	case facts.ResponsesRoute:
		return providers.ServesResponsesIn(menu, p, wire)
	}
	return true
}

// foldAuto turns a typed auto model into AutoModel, so the route runs untyped; a <row>:auto prefix leads on that row.
func foldAuto(o RunOptions) RunOptions {
	prefix, ok := SplitAuto(o.Model)
	if !ok {
		return o
	}
	o.Model, o.AutoModel = "", true
	if prefix != "" {
		o.Provider = prefix
	}
	return o
}

// agyAliasQuery is a typed alias on gemini as agy runs it: the Claude id it serves, else the row's tier.
func agyAliasQuery(q ModelQuery) (ModelQuery, bool) {
	id, ok := models.Alias(strings.ToLower(q.Model))
	if q.Agent != agentid.Gemini || !ok || q.Profile != "" || q.Provider != "" || q.Account != "" ||
		len(models.AcceptedEfforts(agentid.Gemini, id)) == 0 {
		return q, false
	}
	q.Model = id
	return q, true
}
