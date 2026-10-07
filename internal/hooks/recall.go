package hooks

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/textsim"
)

// Recall caps: under 4 distinct terms BM25 ranks noise; the total bounds the per-turn context cost.
const (
	recallMinTerms      = 4
	recallQueryCapRunes = 2000
	recallK             = 3
	recallHitCapRunes   = 400
	recallTotalCapChars = 1500
	recallScoreFloor    = 0.5 // keep hits within half of the top score: BM25 is unnormalised
	// A hit must match a quarter of the query's content terms, clamped: one shared word is coincidence.
	recallMinMatched = 2
	recallMaxMatched = 3
)

// MemoryRecall (UserPromptSubmit) injects the top memory-bank blocks for the prompt; silent on a slash
// command, a short prompt, or no match.
func MemoryRecall(r io.Reader, stdout io.Writer) int {
	in := ParseInput(r)
	q := strings.TrimSpace(in.Prompt)
	if q == "" || strings.HasPrefix(q, "/") || memory.IsHarvestChild() {
		return ExitProceed
	}
	if rs := []rune(q); len(rs) > recallQueryCapRunes {
		q = string(rs[:recallQueryCapRunes])
	}
	if distinctTerms(q) < recallMinTerms {
		return ExitProceed
	}
	root := in.CWD
	if root == "" {
		root, _ = os.Getwd()
	}
	hits := relevantHits(in.SessionID, q, memory.SearchBanks(root, q, 0))
	if len(hits) == 0 {
		return ExitProceed
	}
	var b strings.Builder
	b.WriteString("<!-- memory-recall: bank blocks matching this prompt; more: claude-code memory search -->")
	for _, h := range hits {
		line := strings.Join(strings.Fields(h.Block), " ")
		if rs := []rune(line); len(rs) > recallHitCapRunes {
			line = string(rs[:recallHitCapRunes]) + "…"
		}
		fmt.Fprintf(&b, "\n- [%s/%s] %s", h.Bank, h.File, line)
	}
	memory.MarkSeen(in.SessionID, hits)
	if out := MergeContext(hookspec.EventUserPromptSubmit, []string{memory.CapChars(b.String(), recallTotalCapChars)}); out != "" {
		fmt.Fprintln(stdout, out)
	}
	return ExitProceed
}

// relevantHits keeps up to recallK hits that match enough query terms, score within the floor of the best such
// hit, and are not already in this session's context (SessionStart's surface or an earlier recall).
func relevantHits(sessionID, q string, ranked []memory.SearchHit) []memory.SearchHit {
	need := min(max(recallMinMatched, (memory.QueryTermCount(q)+3)/4), recallMaxMatched)
	seen := memory.SeenBlocks(sessionID)
	var out []memory.SearchHit
	floor := -1.0
	for _, h := range ranked {
		if h.Matched < need {
			continue
		}
		if floor < 0 {
			floor = h.Score * recallScoreFloor
		}
		if h.Score < floor || len(out) == recallK {
			break
		}
		if !seen[memory.BlockKey(h.Block)] {
			out = append(out, h)
		}
	}
	return out
}

// distinctTerms counts distinct tokens of ≥3 chars — a coarse gate on whether to search, never on ranking.
func distinctTerms(q string) int {
	seen := map[string]bool{}
	for _, t := range textsim.Tokenize(q) {
		if len(t) >= 3 {
			seen[t] = true
		}
	}
	return len(seen)
}
