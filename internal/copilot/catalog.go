package copilot

import (
	"context"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/copilot/sdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

// ListModels lists the GitHub Copilot catalog through the SDK on the launch account's token (its own login, never BYOK).
func ListModels(ctx context.Context) ([]providers.ModelInfo, error) {
	bin, err := paths.AgentBin("copilot")
	if err != nil {
		return nil, fmt.Errorf("copilot CLI not found: %w", err)
	}
	list, err := sdk.ListModels(ctx, sdk.CatalogOptions(bin, CopilotToken()))
	if err != nil {
		return nil, err
	}
	out := make([]providers.ModelInfo, len(list))
	for i, m := range list {
		out[i] = providers.ModelInfo{ID: m.ID, Name: m.Name, UseCase: models.UseCase(m.ID, m.Name), Capabilities: copilotCapabilities(m)}
	}
	return out, nil
}

// copilotCapabilities joins copilot's capability flags and premium-billing multiplier into one
// "vision · reasoning · x2" string; "" when the model carries none of them.
func copilotCapabilities(m sdk.Model) string {
	var parts []string
	if m.Vision {
		parts = append(parts, "vision")
	}
	if m.ReasoningEffort {
		parts = append(parts, "reasoning")
	}
	if m.Multiplier != nil {
		parts = append(parts, fmt.Sprintf("x%g", *m.Multiplier))
	}
	return strings.Join(parts, " · ")
}
