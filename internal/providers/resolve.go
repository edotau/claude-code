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
}

// Spec is the "provider:model" form Claude Code sees for a cross-provider slot and the router parses back.
func (t Target) Spec() string { return t.Provider.Name + ":" + t.Model }

// Selection is a session's provider plus each slot's resolved target.
type Selection struct {
	Provider *Provider
	Slots    map[string]Target
}

// ParseTarget splits "provider:model" when the prefix names a registered provider; otherwise the
// whole string is a model on def (ollama tags like "qwen3:30b" keep their colon).
func ParseTarget(reg *Registry, spec string, def *Provider) Target {
	spec = models.Strip1M(strings.TrimSpace(spec))
	if name, model, ok := strings.Cut(spec, ":"); ok {
		if p, found := reg.Providers[name]; found && model != "" {
			return Target{Provider: p, Model: model}
		}
	}
	return Target{Provider: def, Model: spec}
}

// Select resolves the session: provider = flag → HARNESS_PROVIDER pin → registry default; each slot =
// HARNESS_<SLOT>_MODEL → HARNESS_MODEL → the provider's model table.
func Select(reg *Registry, providerFlag string) (Selection, error) {
	pin, err := pinLookup()
	if err != nil {
		return Selection{}, fmt.Errorf("vault: pins unreadable (another process may hold the lock): %w", err)
	}
	name := providerFlag
	if name == "" {
		name = pin(PinProvider)
	}
	if name == "" {
		name = reg.Default
	}
	p, err := reg.Get(name)
	if err != nil {
		return Selection{}, err
	}
	sel := Selection{Provider: p, Slots: map[string]Target{}}
	all := pin(PinModel)
	for _, slot := range models.Slots {
		spec := pin(SlotPin(slot))
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
		return sel, fmt.Errorf("provider %s: no opus model (set models.opus with `claude-code vault edit` or %s)", p.Name, SlotPin(models.Opus))
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
		return t.Spec()
	}
	if t.Provider.OneM {
		return models.MaybeAdd1M(t.Model)
	}
	return t.Model
}

// NeedsRouter reports whether the session must go through the loopback router: a slot crosses
// providers, the provider has no Anthropic route (needs translation), or a failover ladder is configured.
func (s Selection) NeedsRouter(reg *Registry) bool {
	if _, ok := s.Provider.Route(DialectAnthropic); !ok {
		return true
	}
	for _, t := range s.Slots {
		if t.Provider != s.Provider || PrefersOpenAI(t.Provider, t.Model) {
			return true
		}
	}
	// A login session stays direct even with a fallback ladder: a loopback ANTHROPIC_BASE_URL disables Remote Control.
	return len(reg.Fallback) > 0 && s.Provider.Auth.Type != AuthPassthrough
}

// PrefersOpenAI reports whether model must use p's OpenAI route even though p has an Anthropic one:
// a non-Claude model on a provider serving both (one gateway, two paths) needs the router's translation.
func PrefersOpenAI(p *Provider, model string) bool {
	if _, ok := p.Route(DialectAnthropic); !ok {
		return false
	}
	if _, ok := p.Route(DialectOpenAI); !ok {
		return false
	}
	return !strings.Contains(strings.ToLower(model), "claude")
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
