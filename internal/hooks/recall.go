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
	hits := memory.SearchBanks(root, q, recallK)
	if len(hits) == 0 {
		return ExitProceed
	}
	var b strings.Builder
	b.WriteString("<!-- memory-recall: bank blocks matching this prompt; more: claude-code memory search -->")
	for _, h := range hits {
		if h.Score < hits[0].Score*recallScoreFloor {
			break
		}
		line := strings.Join(strings.Fields(h.Block), " ")
		if rs := []rune(line); len(rs) > recallHitCapRunes {
			line = string(rs[:recallHitCapRunes]) + "…"
		}
		fmt.Fprintf(&b, "\n- [%s/%s] %s", h.Bank, h.File, line)
	}
	if out := MergeContext(hookspec.EventUserPromptSubmit, []string{memory.CapChars(b.String(), recallTotalCapChars)}); out != "" {
		fmt.Fprintln(stdout, out)
	}
	return ExitProceed
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
