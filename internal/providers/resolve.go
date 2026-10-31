package providers

import (
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// Target is one resolved (provider, wire model id) pair.
type Target struct {
	Provider *Provider
	Model    string // plain wire id, never [1m]-marked
	OneM     bool   // the user typed [1m]; ClientModel keeps it as typed
}

// ClientModel is t's id on its own provider: [1m]-marked only as the user typed it.
func (t Target) ClientModel() string {
	if t.OneM {
		return t.Model + oneMMarker
	}
	return t.Model
}

// Spec is the "provider:model" form Claude Code sees for a cross-provider slot and the router parses back.
func (t Target) Spec() string { return t.Provider.Name + ":" + t.Model }

// Selection is a session's provider plus each slot's resolved target.
type Selection struct {
	Provider *Provider
	Slots    map[string]Target
}

// ParseTarget splits "provider:model" when the prefix names a registered provider; otherwise the
// whole string is a model on def (local tags like "qwen3:30b" keep their colon).
func ParseTarget(reg *Registry, spec string, def *Provider) Target {
	spec = strings.TrimSpace(spec)
	if name, model, ok := strings.Cut(spec, ":"); ok {
		if p, found := reg.Providers[name]; found && model != "" {
			base, oneM := models.Canonical(model)
			return Target{Provider: p, Model: p.WireModel(base), OneM: oneM}
		}
	}
	base, oneM := models.Canonical(spec)
	return Target{Provider: def, Model: def.WireModel(base), OneM: oneM}
}

// Select resolves the session: profile = flag → CLAUDE_PROFILE pin → registry default; each slot =
// CLAUDE_<TIER>_MODEL → CLAUDE_MODEL → the profile's model table.
func Select(reg *Registry, providerFlag string) (Selection, error) {
	recs, err := agentRecords()
	if err != nil {
		return Selection{}, fmt.Errorf("vault: pins unreadable (another process may hold the lock): %w", err)
	}
	name := providerFlag
	if name == "" {
		name = strings.TrimSpace(recs["claude"].Profile)
	}
	if name == "" {
		name = reg.Default
	}
	p, err := reg.Get(name)
	if err != nil {
		return Selection{}, err
	}
	sel := Selection{Provider: p, Slots: map[string]Target{}}
	rec := recs["claude"]
	all := strings.TrimSpace(rec.Model)
	for _, slot := range models.Slots {
		spec := strings.TrimSpace(rec.Slots[slot])
		if spec == "" {
			spec = all
		}
		if spec == "" {
			spec = p.Models[slot]
		}
		if spec == "" {
			continue
		}
		sel.Slots[slot] = ParseTarget(reg, spec, p)
	}
	if _, ok := sel.Slots[models.Opus]; !ok {
		return sel, fmt.Errorf("provider %s: no opus model (set models.opus with `claude-code vault edit` or `claude-code models --pin opus=<id>`)", p.Name)
	}
	return sel, nil
}

// ClientModel is the id Claude Code is given for slot: a same-provider id (with [1m] where the provider
// serves it), or a "provider:model" spec the router resolves.
func (s Selection) ClientModel(slot string) string {
	t, ok := s.Slots[slot]
	if !ok {
		return ""
	}
	if t.Provider != s.Provider {
		return t.Provider.Name + ":" + t.ClientModel()
	}
	return t.ClientModel()
}

// NeedsRouter reports whether the session must go through the loopback router: a slot crosses providers, a model
// rides a wire other than anthropic (AnthropicRoute), the provider has no Anthropic route, or a failover ladder is set.
func (s Selection) NeedsRouter(reg *Registry) bool {
	if _, ok := s.Provider.Route(ProtocolAnthropic); !ok {
		return true
	}
	for _, t := range s.Slots {
		if _, ok := AnthropicRoute(t.Provider, t.Model); t.Provider != s.Provider || !ok {
			return true
		}
	}
	// A login session stays direct even with a fallback ladder: a loopback ANTHROPIC_BASE_URL disables Remote Control.
	return len(reg.Fallback) > 0 && s.Provider.Auth.Type != AuthCLI
}

// FailoverTarget maps a request that failed on from/model to the equivalent model on next: same slot
// when from's table knows the model, else the Claude family, else next's opus.
func FailoverTarget(from *Provider, model string, next *Provider) (Target, bool) {
	slot := from.SlotOf(model)
	if slot == "" {
		slot = models.FamilyOf(model)
	}
	if id := next.Models[slot]; id != "" {
		return Target{Provider: next, Model: id}, true
	}
	if id := next.Models[models.Opus]; id != "" {
		return Target{Provider: next, Model: id}, true
	}
	return Target{}, false
}

// WireFor is the one rule for which wire model uses on p, from p's routes and the catalog: a Claude model takes the
// anthropic route; a Responses model the responses route; anything else the openai route. ok is false, with why, when p
// has no such route.
func WireFor(p *Provider, model string) (wire string, ok bool, why string) {
	has := func(d string) bool { _, ok := p.Route(d); return ok }
	claude := models.FamilyKind(model) == models.FamilyClaude
	switch {
	case claude && has(ProtocolAnthropic):
		return ProtocolAnthropic, true, ""
	case !claude && ServesResponses(p, model):
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

// AnthropicRoute is the anthropic route model rides on p, or false: WireFor picked it, or the model is unservable on a
// non-Databricks row (third-party hosts serve their own models there; Databricks' anthropic route is Claude-only).
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
