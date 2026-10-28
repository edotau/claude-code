package copilotreg

import (
	"context"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// Endpoint is one serving endpoint copilot can reach; Tested marks endpoints on the curated menu.
type Endpoint struct {
	providers.ModelInfo
	Tested bool
}

// Discover lists the resolved Databricks profile p's READY chat endpoints, marking the curated-menu ones.
func Discover(ctx context.Context, reg *providers.Registry, p *providers.Provider) ([]Endpoint, error) {
	infos, err := providers.ListModels(ctx, reg, p)
	if err != nil {
		return nil, err
	}
	return ServingChatRows(infos, providers.OpencodeMenu()), nil
}

// ServingChatRows keeps READY llm/v1/chat endpoints in order, marking those on menu (with or without "databricks-").
func ServingChatRows(infos []providers.ModelInfo, menu []providers.OpencodeModel) []Endpoint {
	onMenu := make(map[string]bool, len(menu))
	for _, m := range menu {
		onMenu[m.ID] = true
	}
	var out []Endpoint
	for _, info := range infos {
		if info.Task != "llm/v1/chat" || info.Ready != "READY" {
			continue
		}
		tested := onMenu[info.ID] || onMenu[strings.TrimPrefix(info.ID, models.DatabricksPrefix)]
		out = append(out, Endpoint{ModelInfo: info, Tested: tested})
	}
	return out
}

// NonClaudeWire: passthrough wire (openai|responses) + canonical id for a non-Claude model on a routed Databricks row.
func NonClaudeWire(p *providers.Provider, dialect, model string, routed bool) (wire, modelID string) {
	if !routed || p.Preset != providers.PresetDatabricks || dialect != providers.ProtocolAnthropic {
		return "", ""
	}
	canonical, _, ok := models.Resolve(model)
	if !ok {
		canonical = strings.TrimPrefix(model, models.DatabricksPrefix)
	}
	if strings.HasPrefix(canonical, "claude-") {
		return "", ""
	}
	e, _ := models.Lookup(canonical)
	if e.ResponsesOnly || providers.ServesResponses(p, model) {
		return providers.ProtocolResponses, canonical
	}
	return providers.ProtocolOpenAI, canonical
}

// MaxOutputTokens is the output cap current Claude/GPT tiers accept; a catalog-less id needs one.
const MaxOutputTokens = 32_000

// MaxPromptTokens is the window minus the output cap and a 10% margin, so copilot compacts before the endpoint
// refuses; 0 (unset) when the window is too small for that.
func MaxPromptTokens(window int) int {
	if window <= MaxOutputTokens {
		return 0
	}
	return window - MaxOutputTokens - window/10
}

// WellKnownModel maps a gateway Claude id to copilot's catalog form (claude-sonnet-5.5); other ids pass through.
func WellKnownModel(id string) string {
	canonical, _, ok := models.Resolve(id)
	if !ok || !strings.HasPrefix(canonical, "claude-") {
		return id
	}
	return models.Wire("dotted", canonical)
}
