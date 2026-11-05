// agentRecallBlock: the <memory-recall> pointer block appended to a subagent spawn's prompt inside the
// agent-effort hook — a spawned subagent gets no UserPromptSubmit, so this is its only recall surface.
package memory

import (
	"fmt"
	"strings"

	memrecall "github.com/edotau/claude-code/internal/memory/recall"
)

// Same relevance floor as recall.go's recallMinTerms; a one-line preview stays short, the whole
// block stays well under additionalContext's cost and the spec's 1KB cap.
const (
	agentRecallPreviewRunes  = 160
	AgentRecallBlockCapBytes = 1024
)

// AgentRecallMarker guards against appending twice if a caller already attached one.
const AgentRecallMarker = "<memory-recall>"

// AgentRecall ranks root's banks against task and returns a "\n\n<memory-recall>…" block plus the ids
// it cites, for up to 3 hits (persisted id or derived ContentRef — `memory show` loads either);
// ("", nil) on no match.
func AgentRecall(sessionID, cwd, task string) (block string, ids []string) {
	cwd = memrecall.SessionRoot(sessionID, cwd)
	q := strings.TrimSpace(task)
	if q == "" || strings.Contains(q, AgentRecallMarker) {
		return "", nil
	}
	picks, _ := memrecall.RecallPicks(cwd, sessionID, q, false, false)

	var lines []string
	for _, e := range memrecall.RecallPointers(picks) {
		lines = append(lines, "\n- "+recallCut(e.Pointer(), agentRecallPreviewRunes))
		ids = append(ids, e.ID)
	}
	block, n := capRecallLines(lines)
	if block == "" {
		return "", nil
	}
	return block, ids[:n]
}

// capRecallLines assembles the block, dropping whole trailing lines (never cutting one mid-string)
// until it fits AgentRecallBlockCapBytes; ("", 0) when no line (or none alone) fits. n is how many
// lines survived, so the caller can trim its parallel ids slice to match.
func capRecallLines(lines []string) (string, int) {
	const footer = "\nLoad one with: claude-code memory show <id>\n</memory-recall>"
	for len(lines) > 0 {
		block := fmt.Sprintf("\n\n%s%s%s", AgentRecallMarker, strings.Join(lines, ""), footer)
		if len(block) <= AgentRecallBlockCapBytes {
			return block, len(lines)
		}
		lines = lines[:len(lines)-1]
	}
	return "", 0
}
