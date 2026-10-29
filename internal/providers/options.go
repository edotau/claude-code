package providers

import (
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

// Databricks preset wires: serving (default) is /serving-endpoints, ai-gateway is the AI Gateway.
const (
	WireServing   = "serving"
	WireAIGateway = "ai-gateway"
)

// Options are a preset row's non-secret tuning knobs.
type Options struct {
	Wire string `json:"wire,omitempty"` // "" or serving | ai-gateway
}

// gatewayModelPrefix and gatewayModeHeader are the AI Gateway wire-id prefix and the coding-agent header.
const (
	gatewayModelPrefix = "system.ai."
	gatewayModeHeader  = "x-databricks-use-coding-agent-mode"
)

// isGateway reports whether p's preset row selects the AI Gateway wire.
func (p *Provider) isGateway() bool { return p.Options.Wire == WireAIGateway }

// IsAIGateway reports whether p is a Databricks preset row on the AI Gateway wire.
func (p *Provider) IsAIGateway() bool { return p.Preset == PresetDatabricks && p.isGateway() }

// SpellingRule is the models.Wire rule p's model ids use; "" means ids pass through as written.
func (p Provider) SpellingRule() string {
	switch {
	case p.Preset == PresetDatabricks && p.isGateway(), p.ModelPrefix == gatewayModelPrefix: // prefix: legacy Go-built rows
		return "databricks-ai-gateway"
	case p.Preset == PresetDatabricks || p.ModelPrefix == models.DatabricksPrefix:
		return "databricks-serving"
	case p.provider() == "github":
		return "dotted"
	case p.provider() == "anthropic":
		return "anthropic"
	}
	return ""
}

// WireModel spells a catalog model id the way p serves it; idempotent, keeps a trailing [1m], and leaves
// non-catalog and provider-qualified ids as written.
func (p Provider) WireModel(id string) string {
	base, oneM := strings.CutSuffix(id, oneMMarker)
	rule := p.SpellingRule()
	if rule == "" || base == "" || strings.ContainsAny(base, "/:") {
		return id
	}
	canonical, _, ok := models.Resolve(base)
	if !ok {
		return id
	}
	if oneM {
		return models.Wire(rule, canonical) + oneMMarker
	}
	return models.Wire(rule, canonical)
}
