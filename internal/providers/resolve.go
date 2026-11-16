package providers

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// Target is one resolved (provider, wire model id) pair.
type Target struct {
	Provider *Provider
	Model    string // plain wire id, never [1m]-marked
	OneM     bool   // the user typed [1m]; ClientModel keeps it only where models.Keeps1M honours it
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

// ClientModel is the id Claude Code is given for alias: a same-provider id (with [1m] where the provider
// serves it), or a "provider:model" spec the router resolves.
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

// NeedsRouter reports whether the session must go through the loopback router: an alias crosses providers, a model
// rides a non-anthropic wire, the provider lacks an Anthropic route or renames ids, or a failover ladder is set.
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

// FailoverTarget maps a request that failed on from/model to the equivalent model on next: same alias when from's
// table knows the model, else the id's exact Claude version where next spells ids, else its family slot, else opus.
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
