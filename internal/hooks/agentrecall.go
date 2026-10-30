// agentRecallBlock: the <memory-recall> pointer block appended to a subagent spawn's prompt inside the
// agent-effort hook — a spawned subagent gets no UserPromptSubmit, so this is its only recall surface.
package hooks

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// Same relevance floor as recall.go's recallMinTerms; a one-line preview stays short, the whole
// block stays well under additionalContext's cost and the spec's 1KB cap.
const (
	agentRecallQueryCapRunes = 2000
	agentRecallPreviewRunes  = 160
	agentRecallBlockCapBytes = 1024
)

// agentRecallMarker guards against appending twice if a caller already attached one.
const agentRecallMarker = "<memory-recall>"

// agentRecall ranks root's banks against task and returns a "\n\n<memory-recall>…" block plus the ids
// it cites, for up to 3 hits (persisted id or derived ContentRef — `memory show` loads either);
// ("", nil) on no match.
func agentRecall(sessionID, cwd, task string) (block string, ids []string) {
	q := strings.TrimSpace(task)
	if q == "" || strings.Contains(q, agentRecallMarker) {
		return "", nil
	}
	if rs := []rune(q); len(rs) > agentRecallQueryCapRunes {
		q = string(rs[:agentRecallQueryCapRunes])
	}
	tokens := textutil.Tokenize(q)
	if distinctTokenCount(tokens) < recallMinTerms {
		return "", nil
	}
	root := cwd
	if root == "" {
		// A deleted cwd leaves Getwd erroring; fall back to the config dir rather than an empty root.
		if wd, err := os.Getwd(); err == nil {
			root = wd
		} else {
			root = paths.ConfigDir()
		}
	}
	terms := memory.ContentQueryTerms(tokens)
	counts := memory.ReturnCounts([]string{memory.Dir(root), memory.HomeBankDir()}, time.Now())
	picks := selectRecall(sessionID, terms, memory.SearchBanksTerms(root, q, terms, 0), nil, false, counts)

	var lines []string
	for i, p := range picks {
		if i == recallK {
			break
		}
		lines = append(lines, "\n- "+recallCut(p.e.Pointer(), agentRecallPreviewRunes))
		ids = append(ids, p.e.ID)
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
