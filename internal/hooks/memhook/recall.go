package memory

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	memrecall "github.com/edotau/claude-code/internal/memory/recall"
)

func Recall(in *core.Input) core.Result {
	in = sessionRootInput(in)
	picks, trace := memrecall.RecallPicks(in.CWD, in.SessionID, in.Prompt, true, false)
	if after, ok := strings.CutPrefix(trace.Embeddings, "keyword-only: "); ok {
		fmt.Fprintf(os.Stderr, "memory-recall: keyword-only (%s)\n", recallCut(after, 120))
	}
	context, bodies, hits := memrecall.RecallContext(picks)
	if context == "" {
		return core.Result{}
	}
	memrecall.RecordRecall(in.CWD, in.SessionID, bodies, hits)
	return core.Result{Context: context}
}

func recallCut(s string, limit int) string {
	n := 0
	for i := range s {
		if n == limit {
			return s[:i] + "…"
		}
		n++
	}
	return s
}
