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
	name := providerFlag
	if name == "" {
		name = Pin(PinProvider)
	}
	if name == "" {
		name = reg.Default
	}
	p, err := reg.Get(name)
	if err != nil {
		return Selection{}, err
	}
	sel := Selection{Provider: p, Slots: map[string]Target{}}
	all := Pin(PinModel)
	for _, slot := range models.Slots {
		spec := Pin(SlotPin(slot))
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
		return sel, fmt.Errorf("provider %s: no opus model (set models.opus in providers.json or %s)", p.Name, SlotPin(models.Opus))
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
		if t.Provider != s.Provider {
			return true
		}
	}
	return len(reg.Fallback) > 0
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
