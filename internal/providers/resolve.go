package providers

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// Target is one resolved (provider, wire model id) pair.
type Target struct {
	Provider *Provider
	Model    string
	OneM     bool
}

// ClientModel is t's id on its own provider: [1m]-marked only as the user typed it.
func (t Target) ClientModel() string {
	if t.OneM && models.Keeps1M(t.Model+oneMMarker) {
		return t.Model + oneMMarker
	}
	return t.Model
}

// Spec is the "provider:model" form Claude Code sees for a cross-provider alias and the router parses back.
func (t Target) Spec() string { return t.Provider.Name + ":" + t.Model }

// Selection is a session's provider plus each alias's resolved target.
type Selection struct {
	Provider *Provider
	Aliases  map[string]Target
}

// ParseTarget splits "provider:model" by SplitModelSpec; a bad or absent prefix leaves the whole string a model on def.
func ParseTarget(reg *Registry, spec string, def *Provider) Target {
	ms, err := SplitModelSpec(reg, spec)
	p, model := def, strings.TrimSpace(spec)
	if err == nil && ms.Row != "" {
		p, model = reg.Providers[ms.Row], ms.Model
	}
	if IsFamily(model) {
		return Target{Provider: p, Model: ConcreteModel("", p, model)}
	}
	base, oneM := models.Canonical(model)
	return Target{Provider: p, Model: p.WireModel(base), OneM: oneM}
}

// Select resolves the session: profile = flag → record profile → default; alias = record alias → model → table.
func Select(reg *Registry, providerFlag string) (Selection, error) {
	recs, err := agentRecords()
	if err != nil {
		return Selection{}, fmt.Errorf("vault: pins unreadable (another process may hold the lock): %w", err)
	}
	p, err := reg.Get(cmp.Or(providerFlag, strings.TrimSpace(recs["claude"].Profile), reg.Default))
	if err != nil {
		return Selection{}, err
	}
	sel := Selection{Provider: p, Aliases: map[string]Target{}}
	rec := recs["claude"]
	all := strings.TrimSpace(rec.Model)
	for _, alias := range models.AliasNames {
		spec := cmp.Or(strings.TrimSpace(rec.Aliases[alias]), all, p.Models[alias])
		if spec == "" {
			continue
		}
		sel.Aliases[alias] = ParseTarget(reg, spec, p)
	}
	if _, ok := sel.Aliases[models.Opus]; !ok {
		return sel, fmt.Errorf("provider %s: no opus model (pin one with `claude-code models --pin opus=<id>`)", p.Name)
	}
	return sel, nil
}

// ClientModel is the id Claude Code gets for alias: a same-provider id (with [1m] where served) or a router spec.
func (s Selection) ClientModel(alias string) string {
	t, ok := s.Aliases[alias]
	if !ok {
		return ""
	}
	if t.Provider != s.Provider {
		return t.Provider.Name + ":" + t.ClientModel()
	}
	return t.ClientModel()
}

// NeedsRouter reports whether the session needs the loopback router: cross-provider alias, other wire, renamed ids.
func (s Selection) NeedsRouter(reg *Registry) bool {
	if _, ok := s.Provider.Route(ProtocolAnthropic); !ok {
		return true
	}
	// Claude Code's own model ids (the auto-mode classifier's claude-sonnet-5[1m]) reach a renaming wire only rewritten.
	if id, ok := models.Alias(models.Sonnet); ok && s.Provider.WireModel(id) != id {
		return true
	}
	for _, t := range s.Aliases {
		if _, ok := AnthropicRoute(t.Provider, t.Model); t.Provider != s.Provider || !ok {
			return true
		}
	}
	// A login session stays direct even with a fallback ladder: a loopback ANTHROPIC_BASE_URL disables Remote Control.
	return len(reg.Fallback) > 0 && s.Provider.Auth.Type != AuthCLI
}

// FailoverTarget maps a failed from/model to its equivalent on next: same alias, else Claude version, family, opus.
func FailoverTarget(from *Provider, model string, next *Provider) (Target, bool) {
	alias := from.AliasOf(model)
	if alias == "" {
		if canonical, _, ok := models.Resolve(model); ok && models.FamilyOf(canonical) != "" && next.SpellingRule() != "" && next.Provider != "openai" && next.Provider != "google" {
			return Target{Provider: next, Model: next.WireModel(canonical)}, true
		}
		alias = models.FamilyOf(model)
	}
	for _, slot := range []string{alias, models.Opus} {
		if id := next.Models[slot]; id != "" {
			return Target{Provider: next, Model: models.Strip1M(id), OneM: models.Keeps1M(id)}, true
		}
	}
	return Target{}, false
}

// FirstResponsesModel is the first Responses-capable OpencodeMenu entry spelled for p, or "" when none.
func FirstResponsesModel(p *Provider) string {
	for _, m := range OpencodeMenu() {
		if m.Responses {
			return p.WireModel(m.ID)
		}
	}
	return ""
}

// ServesResponses reports whether p serves model on Responses: its menu entry's flag, the catalog, else its family's.
func ServesResponses(p *Provider, model string) bool { return ServesResponsesIn(nil, p, model) }

// ServesResponsesIn is ServesResponses against a menu the caller already read; nil reads it.
func ServesResponsesIn(menu []OpencodeModel, p *Provider, model string) bool {
	if menu == nil {
		menu = OpencodeMenu()
	}
	for _, m := range menu {
		if p.WireModel(m.ID) == model || models.SameModel(m.ID, model) {
			return m.Responses
		}
	}
	if sib, ok := familySibling(model); ok {
		return ResponsesID(sib)
	}
	return models.ResponsesOnly(model)
}

// apiTypeResponses is the listing's api_types entry for the OpenAI Responses API.
const apiTypeResponses = "openai/v1/responses"

// EndpointFacts are one workspace endpoint's listing facts; an empty or zero field is unknown.
type EndpointFacts struct {
	APITypes        []string
	Context, Output int
}

// ListedFacts reads p's fresh listing for wire; the router replaces it, none known until then.
var ListedFacts = func(p *Provider, wire string) (EndpointFacts, bool) { return EndpointFacts{}, false }

// ListedResponses reports whether p's fresh listing says wire serves Responses; known is false with no api_types.
func ListedResponses(p *Provider, wire string) (serves, known bool) {
	if p == nil || p.Preset != PresetDatabricks {
		return false, false
	}
	f, ok := ListedFacts(p, wire)
	if !ok || len(f.APITypes) == 0 {
		return false, false
	}
	return slices.Contains(f.APITypes, apiTypeResponses), true
}

// LimitsFor is wire's token limits: the tracked catalog's value per field, else the fresh listing's, else unknown.
func LimitsFor(p *Provider, wire string) (models.TokenLimits, bool) {
	l, _ := models.Limits(wire)
	if p != nil && (l.Context == 0 || l.Output == 0) {
		if f, ok := ListedFacts(p, wire); ok {
			l.Context, l.Output = cmp.Or(l.Context, f.Context), cmp.Or(l.Output, f.Output)
		}
	}
	return l, l != (models.TokenLimits{})
}

// ResponsesServable reports a wire id codex can run on p: the fresh listing's api_types, else the old family rule.
func ResponsesServable(p *Provider, wire string) bool {
	if serves, known := ListedResponses(p, wire); known {
		return serves
	}
	return (models.FamilyKind(wire) == models.FamilyGPT && !models.ChatOnly(wire)) || ServesResponses(p, wire)
}

// ResponsesID is ServesResponses for a bare menu id: the entry's flag when listed, the catalog, else its family's.
func ResponsesID(id string) bool {
	for _, m := range OpencodeMenu() {
		if models.SameModel(m.ID, id) {
			return m.Responses
		}
	}
	if sib, ok := familySibling(id); ok {
		return ResponsesID(sib)
	}
	return models.ResponsesOnly(id)
}

// WireFor picks the wire model uses on p: Claude anthropic, a Responses model responses, else openai; why when not ok.
func WireFor(p *Provider, model string) (wire string, ok bool, why string) {
	return WireForIn(nil, p, model)
}

// WireForIn is WireFor against a menu the caller already read; nil reads it.
func WireForIn(menu []OpencodeModel, p *Provider, model string) (wire string, ok bool, why string) {
	has := func(d string) bool { _, ok := p.Route(d); return ok }
	claude := models.FamilyKind(model) == models.FamilyClaude
	switch {
	case claude && has(ProtocolAnthropic):
		return ProtocolAnthropic, true, ""
	case !claude && ServesResponsesIn(menu, p, model):
		if has(ProtocolResponses) {
			return ProtocolResponses, true, ""
		}
		return "", false, fmt.Sprintf("%s is Responses-only and provider %s has no responses route", model, p.Name)
	case has(ProtocolOpenAI):
		return ProtocolOpenAI, true, ""
	}
	return "", false, fmt.Sprintf("provider %s has no openai route to serve %s (it has %s)", p.Name, model, strings.Join(p.Dialects(), ","))
}

// ClaudeCodeError is non-nil when Claude Code cannot run model on p: a Responses-only model has no translate path.
func ClaudeCodeError(p *Provider, model string) error {
	if wire, ok, _ := WireFor(p, model); ok && wire == ProtocolResponses {
		return fmt.Errorf("model %s is Responses-only and the router has no Responses translation for Claude Code (provider %s); run it with codex or copilot", model, p.Name)
	}
	return nil
}

// AnthropicRoute is the anthropic route model rides on p, or false when unservable on a non-Databricks row.
func AnthropicRoute(p *Provider, model string) (string, bool) {
	route, has := p.Route(ProtocolAnthropic)
	if !has {
		return "", false
	}
	wire, ok, _ := WireFor(p, model)
	if wire == ProtocolAnthropic || !ok && p.Preset != PresetDatabricks {
		return route, true
	}
	return "", false
}
