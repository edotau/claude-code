// models_pick.go — an agent page's Model row: alias (with what it is for) → that account's model ids → saved.
package configure

import (
	"context"
	"fmt"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/paths"
)

// CopilotListModelsFn is the injectable seam over copilot.SDKListModels; tests fake the SDK catalog.
var CopilotListModelsFn = copilot.SDKListModels

// CopilotModels lists the copilot CLI's live catalog as the profile copilot runs as (its record profile).
func CopilotModels(ctx context.Context) ([]copilot.SDKModel, error) {
	bin, err := paths.AgentBin("copilot")
	if err != nil {
		return nil, fmt.Errorf("copilot CLI not found: %w", err)
	}
	return CopilotListModelsFn(ctx, copilot.CatalogOptions(bin, copilot.CopilotToken()))
}
