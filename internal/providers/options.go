package providers

import "strings"

// Databricks preset wires: serving (default) is /serving-endpoints, gateway is the AI Gateway.
const (
	WireServing = "serving"
	WireGateway = "gateway"
)

// Options are a preset row's non-secret tuning knobs.
type Options struct {
	Wire   string                    `json:"wire,omitempty"`   // "" or serving | gateway
	Models map[string]map[string]any `json:"models,omitempty"` // canonical model id to per-model options; unused
}

// gatewayModelPrefix and gatewayModeHeader are the AI Gateway wire-id prefix and the coding-agent header.
const (
	gatewayModelPrefix = "system.ai."
	gatewayModeHeader  = "x-databricks-use-coding-agent-mode"
)

// isGateway reports whether p's preset row selects the AI Gateway wire.
func (p *Provider) isGateway() bool { return p.Options.Wire == WireGateway }

// presetWireID applies a preset's id rule to prefix+model: dots in the model part become dashes, the prefix stays.
func (p Provider) presetWireID(id string) string {
	if p.Preset != PresetDatabricks {
		return id
	}
	rest, ok := strings.CutPrefix(id, p.ModelPrefix)
	if !ok {
		return id
	}
	return p.ModelPrefix + strings.ReplaceAll(rest, ".", "-")
}
