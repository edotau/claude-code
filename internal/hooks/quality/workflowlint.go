package quality

import (
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/workflows"
)

func WorkflowResult(in *core.Input) core.Result {
	return workflows.WorkflowResult(in)
}
