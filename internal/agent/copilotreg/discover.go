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

// EndpointWire is the passthrough wire for model on the Databricks row p (providers.WireFor) and its canonical id; a
// Claude model, or any model on another row, rides anthropic. ok is false, with why, when p has no route serving it.
func EndpointWire(p *providers.Provider, model string) (wire, canonical string, ok bool, why string) {
	canonical = canonicalOf(model)
	if p.Preset != providers.PresetDatabricks || models.FamilyKind(canonical) == models.FamilyClaude {
		return providers.ProtocolAnthropic, canonical, true, ""
	}
	wire, ok, why = providers.WireFor(p, model)
	return wire, canonical, ok, why
}

// MaxOutputTokens is the output cap current Claude/GPT tiers accept; a catalog-less id needs one.
const MaxOutputTokens = 32_000

// PromptTokens is the window minus the output cap and a 10% margin, so copilot compacts before the endpoint
// refuses; 0 (unset) when the window is too small for that.
func PromptTokens(window, output int) int {
	if window <= output {
		return 0
	}
	return window - output - window/10
}

// Caps returns id's context window and output cap: window and MaxOutputTokens, lowered by any documented catalog limit.
func Caps(window int, id string) (ctx, output int) {
	ctx, output = window, MaxOutputTokens
	l, ok := models.Limits(id)
	if !ok {
		return ctx, output
	}
	if l.Context > 0 { // catalog limits only lower the gated window and the output cap, never raise them
		ctx = min(ctx, l.Context)
	}
	if l.Output > 0 {
		output = min(output, l.Output)
	}
	return ctx, output
}
