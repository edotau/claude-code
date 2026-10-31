package hooks

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
)

func memoryRecall(in *Input) Result {
	picks, trace := memory.RecallPicks(in.CWD, in.SessionID, in.Prompt, true, false)
	if strings.HasPrefix(trace.Embeddings, "keyword-only: ") {
		fmt.Fprintf(os.Stderr, "memory-recall: keyword-only (%s)\n", recallCut(strings.TrimPrefix(trace.Embeddings, "keyword-only: "), 120))
	}
	context, bodies, hits := memory.RecallContext(picks)
	if context == "" {
		return Result{}
	}
	memory.RecordRecall(in.CWD, in.SessionID, bodies, hits)
	return Result{Context: context}
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
