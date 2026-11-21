package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// ModelRoute is one agent and typed model string resolved: the overlay it implies and the wire id that runs.
type ModelRoute struct {
	Agent    string `json:"agent"`
	Provider string `json:"provider"`           // the row's provider type
	Profile  string `json:"profile"`            // registry row
	Account  string `json:"account"`            // Provider.Account(): the ran-on account·profile
	Login    string `json:"login,omitempty"`    // GitHub user on a github row
	Model    string `json:"model"`              // wire id as sent, no [1m]
	OneM     bool   `json:"one_m,omitempty"`    // [1m] typed and honoured by row and model
	Effort   string `json:"effort,omitempty"`   // gemini's suffix cut off Model, or the query's effort
	Dialect  string `json:"dialect"`            // anthropic|openai|responses|gemini, "own-login" for an agent sign-in
	Alias    string `json:"alias,omitempty"`    // the alias typed, when one was
	Source   string `json:"source"`             // prefix|type|flag|account|pool|session|pin|default|own-login
	Identity string `json:"identity,omitempty"` // the overlay's identity, echoed
	Host     string `json:"host,omitempty"`
	Remote   string `json:"remote,omitempty"`
	Fallback string `json:"fallback,omitempty"` // the overlay's fallback mode, echoed
	Served   string `json:"served,omitempty"`   // Databricks row only: ServedNo (fresh listing lacks Model) or ServedUnverified
	Note     string `json:"note,omitempty"`     // a substitution or skip said aloud
}

// Line is the one-line summary: agent · account · model, the effort, the source and any note.
func (r ModelRoute) Line() string {
	line := fmt.Sprintf("%s · %s · %s", r.Agent, r.Account, r.Model)
	if r.Effort != "" {
		line += " effort " + r.Effort
	}
	line += " (" + r.Source + ")"
	if r.Served != "" {
		line += " served: " + r.Served
	}
	if r.Note != "" {
		line += " note: " + r.Note
	}
	return line
}

// Served values: the row's fresh listing lacks the model, or no fresh listing exists to judge by.
const (
	ServedNo         = "no"
	ServedUnverified = "unverified"
)

// ModelQuery is what a launch or dispatch typed, then the fallbacks QueryFor fills, highest precedence first.
type ModelQuery struct {
	ctx                            context.Context
	Route, PinRoute                route.Route
	Agent, Model, Profile, Default string // Default: the alias used when nothing names a model
	Provider, Account, Effort      string // overlay keys: a provider type, a GitHub account, an effort
	Identity, Fallback             string // overlay keys echoed into the route
	Session, PinProfile, PinModel  string
	Held                           bool // live, detached, resumed, hosted, job or batch: the pool pin never picks
	PinAccount                     string
	Task                           string                     // the brief: ResolveRun sizes its tier when auto picks the model
	Tools                          bool                       // the run calls tools: auto skips a model whose probe refused them
	menu                           *[]providers.OpencodeModel // read once per auto resolution; nil reads per use
	pinErr                         error                      // the untyped run's pinned model carries a profile prefix
	rowSpelled                     bool                       // Model is rowDefault's own spelling: never judged by a listing
}

// menuIn is the menu read for this resolution, nil when none was.
func (q ModelQuery) menuIn() []providers.OpencodeModel {
	if q.menu == nil {
		return nil
	}
	return *q.menu
}

// ErrModelRoute marks a typed model string the agent cannot run as written; callers exit 2.
var ErrModelRoute = errors.New("model route")

// poolName is the provider prefix and overlay profile that mean "the shared Databricks pool".
const poolName = AutoModel

// RecordModelSwapper is an optional driver method: swaps a record model the driver's wire cannot serve.
type RecordModelSwapper interface {
	SwapRecordModel(p *providers.Provider, wire string) string
}

// QueryFor fills q's fallbacks from the agent record and the session profile (no last-used profile).
func QueryFor(ctx context.Context, q ModelQuery) ModelQuery {
	q.ctx = ctx
	rec := vault.GetAgentRecord(q.Agent)
	q.PinRoute = rec.EffectiveRoute()
	q.PinProfile, q.PinAccount = strings.TrimSpace(q.PinRoute.Profile), strings.TrimSpace(q.PinRoute.Account)
	q.PinModel = agentModelPinContext(ctx, q.Agent)
	q.Session = providers.SessionProfileContext(ctx)
	if q.Model == "" { // an old `<profile>:<id>` record or <AGENT>_MODEL is refused, never read as a route (D6)
		q.pinErr = providers.CheckModelShared(q.PinModel)
	}
	return q
}

// ResolveModel is the one rule from agent + typed model string to profile, account and wire id.
func ResolveModel(reg *providers.Registry, q ModelQuery, now time.Time) (ModelRoute, error) {
	if !q.Route.IsZero() {
		q.Provider, q.Profile, q.Account, q.Identity = q.Route.Provider, q.Route.Profile, q.Route.Account, q.Route.Identity
	}
	model, err := resolveModelWord(q)
	if err = cmp.Or(q.pinErr, err); err != nil {
		return ModelRoute{}, fmt.Errorf("%w: %w", ErrModelRoute, err)
	}
	q.Model = model
	bare := q.Provider == providers.GroupDatabricks && q.Profile == "" // overlay profile auto or a bare databricks: the pool
	if (q.Profile == poolName || bare) && reg.Providers[poolName] == nil {
		return resolvePool(reg, q, now)
	}
	p, source, err := routeRow(reg, q)
	if err != nil {
		return ModelRoute{}, err
	}
	return resolveOn(p, source, q, now)
}

// ModelWordResolver is an optional driver method: it turns a typed word such as a model family into a model id.
type ModelWordResolver interface {
	ResolveModelWord(ctx context.Context, model string) (string, error)
}

// ColdWordHinter is an optional driver method: the error for a word the resolver left unchanged (no snapshot).
type ColdWordHinter interface {
	ColdModelWord(model string) error
}

// resolveRunModel is resolveModelWord for a local run; a word left unchanged that the driver knows says how to fix it.
func resolveRunModel(ctx context.Context, o RunOptions) (string, error) {
	q := ModelQuery{Agent: o.Agent, Model: o.Model, ctx: ctx}
	model, err := resolveModelWord(q)
	if err != nil || model != o.Model {
		return model, err
	}
	d, _ := driverFor(o.Agent)
	if h, ok := d.(ColdWordHinter); ok && model != "" {
		return model, h.ColdModelWord(model)
	}
	return model, nil
}

func resolveModelWord(q ModelQuery) (string, error) {
	d, ok := driverFor(q.Agent)
	if w, has := d.(ModelWordResolver); ok && has && q.Model != "" {
		return w.ResolveModelWord(cmp.Or(q.ctx, context.Background()), q.Model)
	}
	return q.Model, nil
}

// routeRow walks the profile precedence: typed sources must serve the agent, implicit ones fall through.
func routeRow(reg *providers.Registry, q ModelQuery) (*providers.Provider, string, error) {
	typed, err := typedRow(reg, q)
	if err != nil {
		return nil, "", err
	}
	if typed.name != "" {
		p, err := reg.Get(typed.name)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %w", ErrModelRoute, err)
		}
		if ok, why := rowServes(q.Agent, p); !ok {
			return nil, "", fmt.Errorf("%w: %s", ErrModelRoute, why)
		}
		return p, typed.src, nil
	}
	if p, src, err := geminiModelRow(reg, q); p != nil || err != nil {
		return p, src, err
	}
	p, src, err := implicitRow(reg, q)
	return p, src, err
}

// geminiModelRow is the row a typed model picks for gemini with no --profile: databricks- the gateway, Gemini Google.
func geminiModelRow(reg *providers.Registry, q ModelQuery) (*providers.Provider, string, error) {
	name := strings.ToLower(q.Model)
	if !agentid.Routing(q.Agent).GoogleOrDatabricks || q.Profile != "" || name == "" {
		return nil, "", nil
	}
	if strings.HasPrefix(name, models.DatabricksPrefix) {
		return geminiGatewayPick(reg, q)
	}
	if models.FamilyKind(name) != models.FamilyGemini || GeminiGatewayRow(reg.Row(q.PinProfile)) {
		return nil, "", nil
	}
	for _, n := range reg.Names() {
		if p := reg.Providers[n]; p.Auth.Login == agentid.Gemini {
			return p, "own-login", nil
		}
	}
	return nil, "", nil
}

// geminiGatewayPick is the first of session, pin and default that is a Databricks gateway row.
func geminiGatewayPick(reg *providers.Registry, q ModelQuery) (*providers.Provider, string, error) {
	for _, c := range []struct {
		p   *providers.Provider
		src string
	}{{reg.Row(q.Session), "session"}, {reg.Row(q.PinProfile), "pin"}, {reg.Row(reg.Default), "default"}} {
		if GeminiGatewayRow(c.p) {
			return c.p, c.src, nil
		}
	}
	return nil, "", fmt.Errorf("%w: %s needs a Databricks row and no session, pin or default is one; pass --profile", ErrModelRoute, q.Model)
}

type namedSource struct{ name, src string }

// typedRow is the highest typed source: the profile (a dispatch cell's row or the flag), the provider, the account.
func typedRow(reg *providers.Registry, q ModelQuery) (namedSource, error) {
	switch {
	case q.Profile != "":
		return namedSource{q.Profile, "flag"}, nil
	case q.Provider != "":
		rows := reg.ProfilesOf(q.Provider)
		if p := accountRow(reg, q.Account); q.Account != "" && p != nil && slices.Contains(rows, p.Name) {
			return namedSource{p.Name, "account"}, nil
		}
		if len(rows) != 1 {
			return namedSource{}, fmt.Errorf("%w: provider %q names %d profiles; name one with profile", ErrModelRoute, q.Provider, len(rows))
		}
		return namedSource{rows[0], "type"}, nil
	case q.Account != "":
		if p := accountRow(reg, q.Account); p != nil {
			return namedSource{p.Name, "account"}, nil
		}
		return namedSource{}, fmt.Errorf("%w: account %q matches no profile or GitHub login", ErrModelRoute, q.Account)
	}
	return namedSource{}, nil
}

// accountRow is the row named name, else the github row whose login is name.
func accountRow(reg *providers.Registry, name string) *providers.Provider {
	if p := reg.Row(name); p != nil {
		return p
	}
	for _, n := range reg.ProfilesOf("github") {
		if reg.Providers[n].Login == name {
			return reg.Providers[n]
		}
	}
	return nil
}

// implicitRow tries the session profile, the record profile, the record account, the default, then the agent's login.
func implicitRow(reg *providers.Registry, q ModelQuery) (*providers.Provider, string, error) {
	cands := []*struct {
		p   *providers.Provider
		src string
	}{{sessionRow(reg, q), "session"}, {reg.Row(q.PinProfile), "pin"}, {accountRow(reg, q.PinAccount), "pin"},
		{reg.Row(reg.Default), "default"}}
	for _, c := range cands {
		if c.p == nil {
			continue
		}
		if ok, _ := rowServes(q.Agent, c.p); ok {
			return c.p, c.src, nil
		}
	}
	for _, n := range reg.Names() {
		if p := reg.Providers[n]; p.Auth.Login == q.Agent {
			return p, "own-login", nil
		}
	}
	return nil, "", fmt.Errorf("%w: no profile serves %s", ErrModelRoute, q.Agent)
}

// ownPinRow is the agent's pinned row when it is the agent's own sign-in; that pin decides, never the session.
func ownPinRow(reg *providers.Registry, q ModelQuery) *providers.Provider {
	for _, p := range []*providers.Provider{reg.Row(q.PinProfile), accountRow(reg, q.PinAccount)} {
		if p != nil && p.Auth.Login == q.Agent {
			return p
		}
	}
	return nil
}

// sessionRow is the session profile's row, none under an own-login pin.
func sessionRow(reg *providers.Registry, q ModelQuery) *providers.Provider {
	if ownPinRow(reg, q) != nil {
		return nil
	}
	return reg.Row(q.Session)
}

// rowServes is Serves without a model, plus agy's gateway rows for gemini and claude's own anthropic sign-in.
func rowServes(agent string, p *providers.Provider) (bool, string) {
	switch {
	case agentid.Routing(agent).GoogleOrDatabricks && GeminiGatewayRow(p):
		return true, ""
	case agentid.Routing(agent).AnthropicRoute && p.Auth.Login != "" && p.Auth.Login != agentid.Claude:
		return false, fmt.Sprintf("profile %s is the %s sign-in and cannot back Claude Code", p.Name, p.Auth.Login)
	case agentid.Routing(agent).AnthropicRoute:
		return true, ""
	}
	return Serves(agent, p, "")
}

// resolveOn resolves q on row p: model, spelling, servability and account.
func resolveOn(p *providers.Provider, source string, q ModelQuery, now time.Time) (ModelRoute, error) {
	selected := route.Pick(q.Route, route.Route{Provider: q.Provider, Profile: q.Profile, Account: q.Account, Identity: q.Identity})
	switch source {
	case "pin":
		selected = q.PinRoute
	case "session", "default", "own-login":
		selected = route.Route{}
	}
	r := ModelRoute{Agent: q.Agent, Provider: p.Provider, Profile: p.Name, Account: p.Account(), Source: source,
		Identity: selected.Identity, Host: selected.Host, Remote: selected.Remote, Fallback: q.Fallback}
	if p.Provider == "github" {
		r.Login = cmp.Or(selected.Account, p.Login)
	}
	if err := routeModel(&r, p, q, now); err != nil {
		return ModelRoute{}, err
	}
	aliasNote(&r)
	r.Effort = cmp.Or(r.Effort, q.Effort)
	return r, routeServes(&r, p, q.menuIn())
}

// aliasNote says aloud a Claude tier word the agent ran as another family's model (codex's opus is a GPT id).
func aliasNote(r *ModelRoute) {
	other := r.Model != AutoModel && models.FamilyKind(r.Model) != models.FamilyClaude // auto is a pick, not a swap
	if r.Alias != "" && other && !strings.Contains(r.Note, r.Model) {
		r.Note = joinNotes(r.Note, fmt.Sprintf("%s is %s's alias for %s here", r.Alias, r.Agent, r.Model))
	}
}

// routeModel picks the typed model, else the record's, else the default alias, then cuts a gemini effort and spells it.
func routeModel(r *ModelRoute, p *providers.Provider, q ModelQuery, now time.Time) error {
	typed, pinned := q.Model, false
	if typed == "" {
		typed, pinned = q.PinModel, q.PinModel != ""
	}
	wire, err := spellTyped(r, p, q, cmp.Or(typed, q.Default, models.Opus), now)
	if err != nil && pinned {
		wire, err = pinFallback(r, p, q, err, now)
	}
	if err != nil {
		return err
	}
	if pinned {
		wire = swapUnservedRecord(r, p, q, wire)
	}
	r.Model = wire
	return nil
}

// swapUnservedRecord runs the driver's swap for a record model Responses does not serve, noting it unless r has a note.
func swapUnservedRecord(r *ModelRoute, p *providers.Provider, q ModelQuery, wire string) string {
	if !agentid.Routing(q.Agent).ResponsesRoute || r.Note != "" {
		return wire
	}
	d, ok := driverFor(q.Agent)
	if !ok {
		return wire
	}
	swapper, ok := d.(RecordModelSwapper)
	if !ok {
		return wire
	}
	swapped := swapper.SwapRecordModel(p, wire)
	if swapped == wire {
		return wire
	}
	r.Note = q.Agent + ": record model " + wire + " is not served over Responses; running " + swapped
	return swapped
}

// pinFallback refuses a failed shell pin naming its variable; a failed record model runs the default with a note.
func pinFallback(r *ModelRoute, p *providers.Provider, q ModelQuery, err error, now time.Time) (string, error) {
	if env, v := providers.EnvModel(cmp.Or(q.ctx, context.Background()), q.Agent); env != "" {
		return "", fmt.Errorf("%s=%s: %w", v, env, err)
	}
	r.Alias, r.Effort = "", ""
	def := cmp.Or(q.Default, models.Opus)
	r.Note = fmt.Sprintf("record model %s does not fit %s; used %s", q.PinModel, p.Name, def)
	return spellTyped(r, p, q, def, now)
}

// spellTyped cuts gemini's effort suffix (any other agent refuses it) and spells the id for p.
func spellTyped(r *ModelRoute, p *providers.Provider, q ModelQuery, typed string, now time.Time) (string, error) {
	bare := models.Strip1M(typed)
	if base, effort, ok := agentid.SplitNativeEffort(bare); ok && base != "" {
		agyOwn := len(models.AcceptedEfforts(q.Agent, base)) > 0 // agy's own Claude and GPT-OSS take an effort suffix too
		if models.FamilyKind(base) == models.FamilyGemini || agyOwn {
			if !agentid.Routing(q.Agent).EffortSuffix {
				return "", fmt.Errorf("%w: %s: effort suffixes are gemini's; use --effort %s", ErrModelRoute, typed, effort)
			}
			typed, r.Effort = base, effort
		}
	}
	if w, ok := codexTierPin(r, p, q, typed, now); ok {
		return w, nil
	}
	if q.rowSpelled {
		return spellID(r, p, typed, now), nil
	}
	return spellModel(r, p, q.Agent, typed, now)
}

// codexTierPin is codex's record model for an opus/sonnet alias when p serves it over Responses, else the tier bucket.
func codexTierPin(r *ModelRoute, p *providers.Provider, q ModelQuery, typed string, now time.Time) (string, bool) {
	name := strings.ToLower(models.Strip1M(typed))
	if !agentid.Routing(q.Agent).ResponsesRoute || q.PinModel == "" || name != models.Opus && name != models.Sonnet {
		return "", false
	}
	w, err := spellModel(&ModelRoute{}, p, q.Agent, q.PinModel, now)
	if err != nil || !providers.ServesResponses(p, w) {
		return "", false
	}
	r.Alias = name
	return w, true
}

// spellAlias spells the tier alias name on p, setting r.Alias and r.OneM.
func spellAlias(r *ModelRoute, p *providers.Provider, agent, typed, name string) (string, error) {
	r.Alias = name
	canonical, _ := models.Alias(name)
	m := cmp.Or(router.DatabricksModel(agent, name, p), p.Models[name], canonical)
	if m == "" {
		return "", fmt.Errorf("%w: profile %s has no %s model", ErrModelRoute, p.Name, name)
	}
	wire := models.Strip1M(p.WireModel(m))
	r.OneM = models.Has1M(typed) && models.Keeps1M(wire+"[1m]") && p.Serves1M()
	return wire, nil
}

// spellModel spells typed on p: alias through the row (Databricks bucket, Models), then the catalog by rule.
func spellModel(r *ModelRoute, p *providers.Provider, agent, typed string, now time.Time) (string, error) {
	name := strings.ToLower(models.Strip1M(typed))
	if providers.IsFamily(name) {
		return providers.ConcreteModel(agent, p, name), nil
	}
	if slices.Contains(models.AliasNames, name) {
		return spellAlias(r, p, agent, typed, name)
	}
	if p.Preset != providers.PresetDatabricks && strings.HasPrefix(name, models.DatabricksPrefix) {
		return "", fmt.Errorf("%w: %s is a Databricks endpoint; profile %s is not Databricks", ErrModelRoute, typed, p.Name)
	}
	if _, err := AcceptModel(agent, p, typed, now); err != nil {
		return "", err // an *UnacceptedError, which already wraps ErrModelRoute
	}
	return spellID(r, p, typed, now), nil
}

// spellID is an accepted id's wire spelling on p: the catalog's, else the listing's on Databricks, else as typed.
func spellID(r *ModelRoute, p *providers.Provider, typed string, now time.Time) string {
	if canonical, oneM, ok := models.Resolve(typed); ok {
		r.OneM = oneM && p.Serves1M()
		return models.Strip1M(p.WireModel(canonical))
	}
	if p.Preset == providers.PresetDatabricks {
		return router.SpellServed(p, typed, now)
	}
	return typed
}

// routeServes checks the agent speaks the wire p uses for r.Model.
func routeServes(r *ModelRoute, p *providers.Provider, menu []providers.OpencodeModel) error {
	if r.Model == AutoModel {
		r.Dialect = p.Protocol
		return nil
	}
	if p.Auth.Login == r.Agent {
		return ownLoginServes(r, p)
	}
	if agentid.Routing(r.Agent).AnthropicRoute {
		if err := providers.ClaudeCodeError(p, r.Model); err != nil {
			return fmt.Errorf("%w: %w", ErrModelRoute, err)
		}
	}
	wire, ok, why := providers.WireForIn(menu, p, r.Model)
	if !ok && unlistedAnthropic(r, p) {
		wire, ok = providers.ProtocolAnthropic, true // an unlisted id goes out as typed on the anthropic wire
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrModelRoute, why)
	}
	r.Dialect = wire
	if agentid.Routing(r.Agent).GoogleOrDatabricks && GeminiGatewayRow(p) {
		return geminiGatewayServes(r, p, wire)
	}
	return menuServes(r, p, menu)
}

// ownLoginServes is routeServes for a row the agent signs in to itself.
func ownLoginServes(r *ModelRoute, p *providers.Provider) error {
	r.Dialect = "own-login"
	if err := agyEffort(r); err != nil {
		return err
	}
	return agentFamily(r, p)
}

// menuServes is routeServes' tail: non-anthropic agents must find r.Model on the menu.
func menuServes(r *ModelRoute, p *providers.Provider, menu []providers.OpencodeModel) error {
	if !agentid.Routing(r.Agent).AnthropicRoute {
		if ok, why := servesIn(menu, r.Agent, p, r.Model); !ok {
			return fmt.Errorf("%w: %s", ErrModelRoute, why)
		}
	}
	return agentFamily(r, p)
}

// unlistedAnthropic reports an anthropic-route agent on a row with an anthropic wire and a non-claude/gpt/gemini model.
func unlistedAnthropic(r *ModelRoute, p *providers.Provider) bool {
	_, has := p.Route(providers.ProtocolAnthropic)
	if !has || !agentid.Routing(r.Agent).AnthropicRoute {
		return false
	}
	return !slices.Contains([]models.Family{models.FamilyClaude, models.FamilyGPT, models.FamilyGemini}, models.FamilyKind(r.Model))
}

// geminiGatewayServes is routeServes for agy on a Databricks gateway row, where wire is the model's own wire.
func geminiGatewayServes(r *ModelRoute, p *providers.Provider, wire string) error {
	if wire == providers.ProtocolResponses { // chat/completions refuses these models function tools
		return fmt.Errorf("%w: %s is Responses-only and agy speaks chat/completions; run it with codex, copilot or opencode",
			ErrModelRoute, r.Model)
	}
	switch _, native := p.Route(providers.ProtocolGemini); {
	case models.FamilyKind(r.Model) != models.FamilyGemini:
		r.Dialect = providers.ProtocolOpenAI // agy's openai wire: every family rides chat/completions
	case native:
		r.Dialect = providers.ProtocolGemini // agy's genai wire, as internal/gemini configures it
	}
	return agentFamily(r, p)
}

// agyEffort refuses an effort agy does not offer for r.Model on its own sign-in, naming the accepted ones.
func agyEffort(r *ModelRoute) error {
	accepted := models.AcceptedEfforts(r.Agent, r.Model)
	if r.Effort == "" || len(accepted) == 0 || slices.Contains(accepted, r.Effort) {
		return nil
	}
	return fmt.Errorf("%w: %s on %s takes effort %s, not %q", ErrModelRoute, r.Model, r.Agent, strings.Join(accepted, ", "), r.Effort)
}

// geminiFamilyBad: a tier alias keeps agy's Gemini default; its own Claude and GPT-OSS run on its own sign-in only.
func geminiFamilyBad(r *ModelRoute, p *providers.Provider, fam models.Family) bool {
	if fam == models.FamilyGemini {
		return false
	}
	return r.Alias != "" || !GeminiGatewayRow(p) && len(models.AcceptedEfforts(r.Agent, r.Model)) == 0
}

// agentFamily refuses a model the agent's binary cannot run; gemini runs a named non-Gemini id on Databricks.
func agentFamily(r *ModelRoute, p *providers.Provider) error {
	fam := models.FamilyKind(r.Model)
	bad := false
	switch facts := agentid.Routing(r.Agent); {
	case facts.ModelFamily == string(models.FamilyGemini):
		bad = geminiFamilyBad(r, p, fam)
	case facts.ModelFamily == string(models.FamilyClaude):
		bad = (fam == models.FamilyGPT || fam == models.FamilyGemini) && p.Provider == "anthropic"
	case facts.ResponsesRoute:
		bad = p.Auth.Login == agentid.Codex && (fam == models.FamilyClaude || fam == models.FamilyGemini)
	}
	if bad {
		return fmt.Errorf("%w: %s is not served to %s on profile %s", ErrModelRoute, r.Model, r.Agent, p.Name)
	}
	return nil
}

// Overlay is r as the keyed overlay document a dispatch carries.
func (r ModelRoute) Overlay() agentconfig.Overlay {
	o := agentconfig.Overlay{Schema: 1, Provider: r.Provider, Profile: r.Profile, Model: r.Model, Effort: r.Effort,
		Fallback: r.Fallback, Identity: r.Identity, Host: r.Host, Remote: r.Remote}
	if r.Provider == "github" {
		o.Account = cmp.Or(r.Login, r.Profile)
	}
	return o
}

// Label is the "<model>" of a dispatch description: the wire id the ran-on line prints.
func (r ModelRoute) Label() string { return r.Model }

// resolvePool is the provider pool's first usable profile serving the model, ranked least busy first.
func resolvePool(reg *providers.Registry, q ModelQuery, now time.Time) (ModelRoute, error) {
	group := cmp.Or(q.Provider, providers.GroupDatabricks)
	st, skipped := router.LoadState(), []string(nil)
	names := reg.ProfilesOf(group)
	if len(names) == 0 {
		return ModelRoute{}, fmt.Errorf("%w: provider %q has no profiles configured", ErrModelRoute, group)
	}
	names = autoOrder(reg, group, names, st, q.Model == AutoModel || q.Model == "", now)
	for _, name := range names {
		r, skip, err := poolProfile(reg, q, name, st, now)
		if err != nil {
			return ModelRoute{}, err
		}
		if skip != "" {
			skipped = append(skipped, skip)
			continue
		}
		if len(skipped) > 0 {
			r.Note = strings.TrimSpace("skipped " + strings.Join(skipped, "; ") + ". " + r.Note)
		}
		return r, nil
	}
	label := group
	if group == providers.GroupDatabricks {
		label = "Databricks"
	}
	return ModelRoute{}, fmt.Errorf("%w: no %s profile can serve %s for %s (%s)", ErrModelRoute,
		label, cmp.Or(q.Model, q.Default, models.Opus), q.Agent, strings.Join(skipped, "; "))
}

// poolProfile resolves q on one pool profile; a non-empty skip is why that profile cannot serve it.
func poolProfile(reg *providers.Registry, q ModelQuery, name string, st router.State, now time.Time) (ModelRoute, string, error) {
	p, err := agentconfig.CommandProvider(q.ctx, reg.Providers[name])
	if err != nil {
		return ModelRoute{}, "", err
	}
	if ok, why := rowServes(q.Agent, p); !ok {
		return ModelRoute{}, name + ": " + why, nil
	}
	if p.Preset == providers.PresetDatabricks {
		if fs := launchWorkspaceFindings(q.ctx, q.Agent, p, st, now, nil); len(fs) > 0 {
			return ModelRoute{}, name + ": " + fs[0].Detail, nil
		}
	}
	r, err := resolveOn(p, "pool", q, now)
	if err == nil && p.Preset == providers.PresetDatabricks {
		err = poolListed(p, r.Model, now)
	}
	if err != nil {
		return ModelRoute{}, name + ": " + strings.TrimPrefix(err.Error(), ErrModelRoute.Error()+": "), nil
	}
	return r, "", nil
}

// autoOrder is names in <group>:auto's order (cooling last, fresh first when auto, fewest live, least recent pick).
func autoOrder(reg *providers.Registry, group string, names []string, st router.State, preferFresh bool, now time.Time) []string {
	q := router.WorkspaceQuery[struct{}]{
		Provider:    group,
		PreferFresh: preferFresh,
		Findings:    func(*providers.Provider, string, string, time.Time) []struct{} { return nil },
		Unserved:    func(string, []struct{}) error { return nil },
		Cooling: func(p *providers.Provider, now time.Time) bool {
			return st.CoolingUntil(router.AccountOf(p)).After(now)
		},
	}
	ranked, _, err := router.RankWorkspaces(reg, q, now)
	if err != nil {
		return names
	}
	for _, n := range names { // a row the ranking skips follows in vault order
		if !slices.Contains(ranked, n) {
			ranked = append(ranked, n)
		}
	}
	return ranked
}

// poolListed refuses a model p's fresh listing shows unlisted or not READY.
func poolListed(p *providers.Provider, wire string, now time.Time) error {
	if router.ListedNotReady(p, wire, now) {
		return fmt.Errorf("%w: %s is not READY on %s", ErrModelRoute, wire, p.Name)
	}
	return nil
}
