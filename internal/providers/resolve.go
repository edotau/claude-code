package providers

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/serving"
)

// Target is one resolved (provider, wire model id) pair.
type Target struct {
	Provider *Provider
	Model    string
	// ContextTokens is the window the model runs at on Provider; 0 = unknown.
	ContextTokens int
}

// ClientModel is t's id on its own provider: [1m]-marked only as the user typed it.
func (t Target) ClientModel() string {
	if t.ContextTokens >= models.ContextWindow1M && models.Keeps1M(t.Model+oneMMarker) {
		return t.Model + oneMMarker
	}
	return t.Model
}

// Spec is t for display: the wire id and the profile serving it, never a prefixed model string.
func (t Target) Spec() string { return t.Model + " on " + t.Provider.Name }

// Selection is a session's provider plus each alias's resolved target.
type Selection struct {
	Provider *Provider
	Aliases  map[string]Target
}

// PlainTarget is a plain model id on p: a family word as p's member, else p's wire spelling with any [1m] mark kept.
func PlainTarget(p *Provider, model string) Target {
	if IsFamily(model) {
		wire := ConcreteModel("", p, model)
		return Target{Provider: p, Model: wire, ContextTokens: ContextTokens(p, wire, false)}
	}
	base, oneM := models.Canonical(model)
	wire := p.WireModel(base)
	return Target{Provider: p, Model: wire, ContextTokens: ContextTokens(p, wire, oneM)}
}

// prefixedPinNote is the one stderr note per process for an ignored profile-prefixed pin.
var prefixedPinNote sync.Once

// SessionPin is a stored model pin, or "" for an old profile-prefixed one: aliases resolve on the session's profile.
func SessionPin(reg *Registry, pin string) string {
	pin = strings.TrimSpace(pin)
	if CheckModel(reg, pin) != nil {
		prefixedPinNote.Do(func() {
			fmt.Fprintf(os.Stderr, "note: model pin %s names another profile and is ignored; clear only that pin with "+
				"`claude-code models --pin <alias>=` (`all=` for the all-slot pin; `claude-code models preflight` names it)\n", pin)
		})
		return ""
	}
	return pin
}

// Select resolves the session: profile = flag → record profile → default; alias = record alias → model → table.
func Select(reg *Registry, providerFlag string) (Selection, error) {
	recs, err := agentRecords()
	if err != nil {
		return Selection{}, fmt.Errorf("vault: pins unreadable (another process may hold the lock): %w", err)
	}
	p, err := reg.Get(cmp.Or(providerFlag, recordProfile(recs["claude"]), reg.Default))
	if err != nil {
		return Selection{}, err
	}
	sel := Selection{Provider: p, Aliases: map[string]Target{}}
	rec := recs["claude"]
	all := strings.TrimSpace(rec.Model)
	for _, alias := range models.AliasNames {
		spec := cmp.Or(SessionPin(reg, rec.Aliases[alias]), SessionPin(reg, all), p.Models[alias])
		if spec == "" {
			continue
		}
		sel.Aliases[alias] = PlainTarget(p, spec)
	}
	if _, ok := sel.Aliases[models.Opus]; !ok {
		return sel, fmt.Errorf("provider %s: no opus model (pin one with `claude-code models --pin opus=<id>`)", p.Name)
	}
	return sel, nil
}

// ClientModel is the plain id Claude Code gets for alias; every alias resolves on the session's own profile.
func (s Selection) ClientModel(alias string) string {
	t, ok := s.Aliases[alias]
	if !ok {
		return ""
	}
	return t.ClientModel()
}

// NeedsRouter reports whether the session needs the loopback router: other wire, renamed ids.
func (s Selection) NeedsRouter(reg *Registry) bool {
	if _, ok := s.Provider.Route(ProtocolAnthropic); !ok {
		return true
	}
	// Claude Code's own model ids (the auto-mode classifier's claude-sonnet-5[1m]) reach a renaming wire only rewritten.
	if id, ok := models.Alias(models.Sonnet); ok && s.Provider.WireModel(id) != id {
		return true
	}
	for _, t := range s.Aliases {
		if _, ok := AnthropicRoute(t.Provider, t.Model); !ok {
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
			wire := next.WireModel(canonical)
			return Target{Provider: next, Model: wire, ContextTokens: ContextTokens(next, wire, false)}, true
		}
		alias = models.FamilyOf(model)
	}
	for _, slot := range []string{alias, models.Opus} {
		if id := next.Models[slot]; id != "" {
			wire := models.Strip1M(id)
			return Target{Provider: next, Model: wire, ContextTokens: ContextTokens(next, wire, models.Keeps1M(id))}, true
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

// apiTypeResponses and apiTypeCodexResponses are the Databricks listing api_types for streaming Responses support.
const (
	apiTypeResponses      = "openai/v1/responses"
	apiTypeCodexResponses = "codex/v1/responses"
)

func listedFacts(p *Provider, wire string) (serving.EndpointFacts, bool) {
	return serving.Facts(p.Name, p.WireModel(models.Strip1M(strings.TrimSpace(wire))))
}

// ListedResponses reports whether p's fresh listing says wire serves streaming Responses; known=false if absent.
func ListedResponses(p *Provider, wire string) (serves, known bool) {
	if p == nil || p.Preset != PresetDatabricks {
		return false, false
	}
	f, ok := listedFacts(p, wire)
	if !ok || len(f.APITypes) == 0 {
		return false, false
	}
	return slices.Contains(f.APITypes, apiTypeResponses) && slices.Contains(f.APITypes, apiTypeCodexResponses), true
}

// LimitsFor is wire's limits: catalog context else the listing's; a fresh listed output beats the catalog's.
func LimitsFor(p *Provider, wire string) (models.TokenLimits, bool) {
	l, _ := models.Limits(wire)
	if listsEndpoints(p) {
		if f, ok := listedFacts(p, wire); ok {
			l.Context, l.Output = cmp.Or(l.Context, f.Context), cmp.Or(f.Output, l.Output)
		}
	}
	return l, l != (models.TokenLimits{})
}

// ResponsesServable reports a wire id codex and pi can run on p: the fresh listing, else the menu flag or GPT family.
// With no listing it is deliberately wider than WireFor, which ignores the GPT-family rule.
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
	case !claude && wireNeedsResponses(menu, p, model):
		if has(ProtocolResponses) {
			return ProtocolResponses, true, ""
		}
		return "", false, fmt.Sprintf("%s is Responses-only and provider %s has no responses route", model, p.Name)
	case has(ProtocolOpenAI):
		return ProtocolOpenAI, true, ""
	}
	return "", false, fmt.Sprintf("provider %s has no openai route to serve %s (it has %s)", p.Name, model, strings.Join(p.Dialects(), ","))
}

// wireNeedsResponses reports whether model must use Responses on p: live api_types beat stale menu guesses.
func wireNeedsResponses(menu []OpencodeModel, p *Provider, model string) bool {
	if serves, known := ListedResponses(p, model); known {
		return serves
	}
	return ServesResponsesIn(menu, p, model)
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
