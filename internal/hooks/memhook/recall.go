package memhook

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func Recall(in *core.Input) core.Result {
	in = sessionRootInput(in)
	picks, trace := memory.RecallPicks(in.CWD, in.SessionID, in.Prompt, true, false)
	if after, ok := strings.CutPrefix(trace.Embeddings, "keyword-only: "); ok {
		fmt.Fprintf(os.Stderr, "memory-recall: keyword-only (%s)\n", memory.Cut(after, 120))
	}
	context, bodies, hits := memory.RecallContext(picks)
	if context == "" {
		return core.Result{}
	}
	memory.RecordRecall(in.CWD, in.SessionID, bodies, hits)
	return core.Result{Context: context}
}

// AgentRecall: the <memory-recall> pointer block appended to a subagent spawn's prompt inside the
// agent-effort hook — a spawned subagent gets no UserPromptSubmit, so this is its only recall surface.

// Same relevance floor as recall.go's recallMinTerms; a one-line preview stays short, the whole
// block stays well under additionalContext's cost and the spec's 1KB cap.
const (
	agentRecallPreviewRunes  = 160
	agentRecallBlockCapBytes = 1024
)

// agentRecallMarker guards against appending twice if a caller already attached one.
const agentRecallMarker = "<memory-recall>"

// AgentRecall ranks root's banks against task and returns a "\n\n<memory-recall>…" block plus the ids
// it cites, for up to 3 hits (persisted id or derived ContentRef — `memory show` loads either);
// ("", nil) on no match.
func AgentRecall(sessionID, cwd, task string) (block string, ids []string) {
	cwd = memory.SessionRoot(sessionID, cwd)
	q := strings.TrimSpace(task)
	if q == "" || strings.Contains(q, agentRecallMarker) {
		return "", nil
	}
	picks, _ := memory.RecallPicks(cwd, sessionID, q, false, false)

	var lines []string
	for _, e := range memory.RecallPointers(picks) {
		lines = append(lines, "\n- "+memory.Cut(e.Pointer(), agentRecallPreviewRunes))
		ids = append(ids, e.ID)
	}
	block, n := capRecallLines(lines)
	if block == "" {
		return "", nil
	}
	return block, ids[:n]
}

// capRecallLines assembles the block, dropping whole trailing lines (never cutting one mid-string)
// until it fits agentRecallBlockCapBytes; ("", 0) when no line (or none alone) fits. n is how many
// lines survived, so the caller can trim its parallel ids slice to match.
func capRecallLines(lines []string) (string, int) {
	const footer = "\nLoad one with: claude-code memory show <id>\n</memory-recall>"
	for len(lines) > 0 {
		block := fmt.Sprintf("\n\n%s%s%s", agentRecallMarker, strings.Join(lines, ""), footer)
		if len(block) <= agentRecallBlockCapBytes {
			return block, len(lines)
		}
		lines = lines[:len(lines)-1]
	}
	return "", 0
}
