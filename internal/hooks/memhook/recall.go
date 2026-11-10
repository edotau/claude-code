package memhook

import (
	"os"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func Recall(in *core.Input) core.Result {
	return core.Result{Context: memory.PromptRecall(in.SessionID, in.CWD, in.Prompt, os.Stderr)}
}
