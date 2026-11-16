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
	Fallback string `json:"fallback,omitempty"` // the overlay's fallback mode, echoed
	Note     string `json:"note,omitempty"`     // a substitution or skip said aloud
}

// ModelQuery is what a launch or dispatch typed, then the fallbacks QueryFor fills, highest precedence first.
type ModelQuery struct {
	Agent, Model, Profile, Default string // Default: the alias used when nothing names a model
	Provider, Account, Effort      string // overlay keys: a provider type, a GitHub account, an effort
	Identity, Fallback             string // overlay keys echoed into the route
	Session, PinProfile, PinModel  string
	PinAccount                     string
	Interactive                    bool // a prefix and Profile naming different rows is an error, not an override
}

// ErrModelRoute marks a typed model string the agent cannot run as written; callers exit 2.
var ErrModelRoute = errors.New("model route")

// poolName is the provider prefix and overlay profile that mean "the shared Databricks pool".
const poolName = "auto"

// codexSwap is the record-model swap codex applies; internal/codex sets it through SetCodexSwap.
var codexSwap = func(p *providers.Provider, wire string) string { return wire }

// SetCodexSwap installs codex's Responses-only swap for a record model.
func SetCodexSwap(f func(*providers.Provider, string) string) { codexSwap = f }

// QueryFor fills q's fallbacks from the agent record and the session profile (no last-used profile).
func QueryFor(ctx context.Context, q ModelQuery) ModelQuery {
	rec := vault.GetAgentRecord(q.Agent)
	q.PinProfile, q.PinAccount = strings.TrimSpace(rec.Profile), strings.TrimSpace(rec.Account)
	q.PinModel = agentModelPinContext(ctx, q.Agent)
	q.Session = providers.SessionProfileContext(ctx)
	return q
}

// ResolveModel is the one rule from agent + typed model string to profile, account and wire id.
func ResolveModel(reg *providers.Registry, q ModelQuery, now time.Time) (ModelRoute, error) {
	spec, pool, err := splitQuery(reg, q)
	if err != nil {
		return ModelRoute{}, fmt.Errorf("%w: %w", ErrModelRoute, err)
	}
	if pool {
		return resolvePool(reg, q, spec, now)
	}
	p, source, note, err := routeRow(reg, q, spec)
	if err != nil {
		return ModelRoute{}, err
	}
	r, err := resolveOn(p, source, q, spec, now)
	r.Note = strings.Join(slices.DeleteFunc([]string{note, r.Note}, func(s string) bool { return s == "" }), "; ")
	return r, err
}

// splitQuery reads the typed prefix; pool is true for databricks:<model> and overlay profile auto.
func splitQuery(reg *providers.Registry, q ModelQuery) (providers.ModelSpec, bool, error) {
	if name, rest, ok := strings.Cut(strings.TrimSpace(q.Model), ":"); ok && rest != "" && reg.Providers[name] == nil &&
		(name == poolPrefix(reg) || name == poolName) {
		return providers.ModelSpec{Model: rest}, true, nil
	}
	spec, err := providers.SplitModelSpec(reg, q.Model)
	if err != nil {
		return spec, false, err
	}
	bare := q.Provider == providers.GroupDatabricks && q.Profile == ""
	if (q.Profile == poolName || bare) && spec.Row == "" && reg.Providers[poolName] == nil {
		return spec, true, nil
	}
	return spec, false, nil
}

// poolPrefix is the Databricks provider type, the pool's prefix.
func poolPrefix(reg *providers.Registry) string {
	if len(reg.ProfilesOf(providers.GroupDatabricks)) > 0 {
		return providers.GroupDatabricks
	}
	return ""
}

// routeRow walks the profile precedence: typed sources must serve the agent, implicit ones fall through.
func routeRow(reg *providers.Registry, q ModelQuery, spec providers.ModelSpec) (*providers.Provider, string, string, error) {
	note := ""
	if spec.Row != "" && q.Profile != "" && spec.Row != q.Profile {
		if q.Interactive {
			return nil, "", "", fmt.Errorf("%w: --profile %s and --model %s name different profiles", ErrModelRoute, q.Profile, q.Model)
		}
		note = fmt.Sprintf("--model prefix %s overrides profile %s", spec.Row, q.Profile)
	}
	typed, err := typedRow(reg, q, spec)
	if err != nil {
		return nil, "", "", err
	}
	if typed.name != "" {
		p, err := reg.Get(typed.name)
		if err != nil {
			return nil, "", "", fmt.Errorf("%w: %w", ErrModelRoute, err)
		}
		if ok, why := rowServes(q.Agent, p); !ok {
			return nil, "", "", fmt.Errorf("%w: %s", ErrModelRoute, why)
		}
		return p, typed.src, note, nil
	}
	p, src, err := implicitRow(reg, q)
	return p, src, note, err
}

type namedSource struct{ name, src string }

// typedRow is the highest typed source: prefix or type, the profile flag, the overlay provider, the account.
func typedRow(reg *providers.Registry, q ModelQuery, spec providers.ModelSpec) (namedSource, error) {
	switch {
	case spec.Row != "":
		return namedSource{spec.Row, cmp.Or(map[bool]string{true: "type"}[spec.ByType], "prefix")}, nil
	case q.Profile != "":
		return namedSource{q.Profile, "flag"}, nil
	case q.Provider != "":
		rows := reg.ProfilesOf(q.Provider)
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
	}{{reg.Row(q.Session), "session"}, {reg.Row(q.PinProfile), "pin"}, {accountRow(reg, q.PinAccount), "pin"},
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

// rowServes is Serves without a model, plus agy's gateway rows for gemini and claude's own anthropic sign-in.
func rowServes(agent string, p *providers.Provider) (bool, string) {
	switch {
	case agent == agentid.Gemini && GeminiGatewayRow(p):
		return true, ""
	case agent == agentid.Claude && p.Auth.Login != "" && p.Auth.Login != agentid.Claude:
		return false, fmt.Sprintf("profile %s is the %s sign-in and cannot back Claude Code", p.Name, p.Auth.Login)
	case agent == agentid.Claude:
		return true, ""
	}
	return Serves(agent, p, "")
}

// resolveOn resolves q on row p: model, spelling, servability and account.
func resolveOn(p *providers.Provider, source string, q ModelQuery, spec providers.ModelSpec, now time.Time) (ModelRoute, error) {
	r := ModelRoute{Agent: q.Agent, Provider: p.Provider, Profile: p.Name, Account: p.Account(), Source: source,
		Identity: q.Identity, Fallback: q.Fallback}
	if p.Provider == "github" {
		r.Login = p.Login
	}
	if err := routeModel(&r, p, q, spec, now); err != nil {
		return ModelRoute{}, err
	}
	r.Effort = cmp.Or(r.Effort, q.Effort)
	return r, routeServes(&r, p)
}

// routeModel picks the typed model, else the record's, else the default alias, then cuts a gemini effort and spells it.
func routeModel(r *ModelRoute, p *providers.Provider, q ModelQuery, spec providers.ModelSpec, now time.Time) error {
	typed, pinned := spec.Model, false
	if typed == "" {
		typed, pinned = q.PinModel, q.PinModel != ""
	}
	wire, err := spellTyped(r, p, q, cmp.Or(typed, q.Default, models.Opus), now)
	if err != nil && pinned {
		r.Alias, r.Effort = "", ""
		wire, err = spellTyped(r, p, q, cmp.Or(q.Default, models.Opus), now)
		r.Note = fmt.Sprintf("record model %s does not fit %s; used %s", typed, p.Name, cmp.Or(q.Default, models.Opus))
	}
	if err != nil {
		return err
	}
	if pinned && q.Agent == agentid.Codex && r.Note == "" {
		if swapped := codexSwap(p, wire); swapped != wire {
			r.Note, wire = "codex: record model "+wire+" is not served over Responses; running "+swapped, swapped
		}
	}
	r.Model = wire
	return nil
}

// spellTyped cuts gemini's effort suffix (any other agent refuses it) and spells the id for p.
func spellTyped(r *ModelRoute, p *providers.Provider, q ModelQuery, typed string, now time.Time) (string, error) {
	bare := models.Strip1M(typed)
	if i := strings.LastIndex(bare, "-"); i > 0 {
		base, effort := bare[:i], bare[i+1:]
		if slices.Contains(agentid.NativeEfforts(agentid.Gemini), effort) && models.FamilyKind(base) == models.FamilyGemini {
			if q.Agent != agentid.Gemini {
				return "", fmt.Errorf("%w: %s: effort suffixes are gemini's; use --effort %s", ErrModelRoute, typed, effort)
			}
			typed, r.Effort = base, effort
		}
	}
	return spellModel(r, p, q.Agent, typed, now)
}

// spellModel spells typed on p: alias through the row (Databricks bucket first), catalog id by rule, else as listed.
func spellModel(r *ModelRoute, p *providers.Provider, agent, typed string, now time.Time) (string, error) {
	name := strings.ToLower(models.Strip1M(typed))
	if slices.Contains(models.AliasNames, name) {
		r.Alias = name
		if m := cmp.Or(router.DatabricksModel(agent, name, p), p.Models[name]); m != "" {
			return models.Strip1M(p.WireModel(m)), nil
		}
		return "", fmt.Errorf("%w: profile %s has no %s model", ErrModelRoute, p.Name, name)
	}
	if p.Preset != providers.PresetDatabricks && strings.HasPrefix(name, models.DatabricksPrefix) {
		return "", fmt.Errorf("%w: %s is a Databricks endpoint; profile %s is not Databricks", ErrModelRoute, typed, p.Name)
	}
	if canonical, oneM, ok := models.Resolve(typed); ok {
		r.OneM = oneM && p.Serves1M()
		return models.Strip1M(p.WireModel(canonical)), nil
	}
	if p.Preset != providers.PresetDatabricks {
		return typed, nil
	}
	if w := router.SpellServed(p, typed, now); w != typed || strings.HasPrefix(typed, models.DatabricksPrefix) {
		return w, nil
	}
	return "", fmt.Errorf("%w: %s is no catalog model and %s lists no READY endpoint for it", ErrModelRoute, typed, p.Name)
}

// routeServes checks the agent speaks the wire p uses for r.Model.
func routeServes(r *ModelRoute, p *providers.Provider) error {
	if r.Model == "auto" {
		r.Dialect = p.Protocol
		return nil
	}
	if p.Auth.Login == r.Agent {
		r.Dialect = "own-login"
		return agentFamily(r, p)
	}
	if r.Agent == agentid.Claude {
		if err := providers.ClaudeCodeError(p, r.Model); err != nil {
			return fmt.Errorf("%w: %w", ErrModelRoute, err)
		}
	}
	wire, ok, why := providers.WireFor(p, r.Model)
	if !ok {
		return fmt.Errorf("%w: %s", ErrModelRoute, why)
	}
	r.Dialect = wire
	if r.Agent == agentid.Claude || r.Agent == agentid.Gemini && GeminiGatewayRow(p) {
		return agentFamily(r, p)
	}
	if ok, why := Serves(r.Agent, p, r.Model); !ok {
		return fmt.Errorf("%w: %s", ErrModelRoute, why)
	}
	return agentFamily(r, p)
}

// agentFamily refuses a model the agent's vendor binary cannot run: claude runs Claude models, gemini Gemini models.
func agentFamily(r *ModelRoute, p *providers.Provider) error {
	fam := models.FamilyKind(r.Model)
	bad := false
	switch r.Agent {
	case agentid.Gemini:
		bad = fam != models.FamilyGemini
	case agentid.Claude:
		bad = fam != models.FamilyClaude && p.Preset != providers.PresetDatabricks
	case agentid.Codex:
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
		Fallback: r.Fallback, Identity: r.Identity}
	if r.Provider == "github" {
		o.Account = r.Profile
	}
	return o
}

// Label is the "<model>" of a dispatch description: the wire id the ran-on line prints.
func (r ModelRoute) Label() string { return r.Model }

// resolvePool is the shared Databricks pool: the first workspace in vault order that is usable and serves the model.
func resolvePool(reg *providers.Registry, q ModelQuery, spec providers.ModelSpec, now time.Time) (ModelRoute, error) {
	st, skipped := router.LoadState(), []string(nil)
	for _, name := range reg.ProfilesOf(providers.GroupDatabricks) {
		p := reg.Providers[name]
		if ok, why := rowServes(q.Agent, p); !ok {
			skipped = append(skipped, name+": "+why)
			continue
		}
		if fs := CheckWorkspace(p, st, now); len(fs) > 0 {
			skipped = append(skipped, name+": "+fs[0].Detail)
			continue
		}
		r, err := resolveOn(p, "pool", q, spec, now)
		if err == nil {
			err = poolListed(p, r.Model, now)
		}
		if err != nil {
			skipped = append(skipped, name+": "+strings.TrimPrefix(err.Error(), ErrModelRoute.Error()+": "))
			continue
		}
		if len(skipped) > 0 {
			r.Note = strings.TrimSpace("skipped " + strings.Join(skipped, "; ") + ". " + r.Note)
		}
		return r, nil
	}
	return ModelRoute{}, fmt.Errorf("%w: no Databricks profile can serve %s for %s (%s)", ErrModelRoute,
		cmp.Or(spec.Model, q.Default, models.Opus), q.Agent, strings.Join(skipped, "; "))
}

// poolListed refuses a model p's fresh listing shows unlisted or not READY.
func poolListed(p *providers.Provider, wire string, now time.Time) error {
	if _, ready, fresh := router.ServedListed(p, wire, now); fresh && !ready {
		return fmt.Errorf("%w: %s is not READY on %s", ErrModelRoute, wire, p.Name)
	}
	return nil
}
