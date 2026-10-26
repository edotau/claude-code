package hooks

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
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
	tokens := textsim.Tokenize(q)
	if distinctTokenCount(tokens) < recallMinTerms {
		return ExitProceed
	}
	root := in.CWD
	if root == "" {
		// A deleted cwd leaves Getwd erroring; fall back to the config dir rather than an empty root.
		if wd, err := os.Getwd(); err == nil {
			root = wd
		} else {
			root = paths.ConfigDir()
		}
	}
	terms := memory.ContentQueryTerms(tokens)
	hits := relevantTermHits(in.SessionID, terms, memory.SearchBanksTerms(root, q, terms, 0))
	if len(hits) == 0 {
		return ExitProceed
	}
	var b strings.Builder
	b.WriteString("<!-- memory-recall: bank blocks matching this prompt; more: claude-code memory search -->")
	now := time.Now()
	access := map[string][]string{}
	for _, h := range hits {
		line := strings.Join(strings.Fields(h.Block), " ")
		line = recallCut(line, recallHitCapRunes)
		fmt.Fprintf(&b, "\n- [%s/%s] %s", h.Bank, h.File, line)
		dir := logDirFor(root, h.Bank)
		access[dir] = append(access[dir], h.Bank+":"+h.File)
	}
	for dir, entries := range access {
		memory.LogAccessBatch(dir, now, entries, "prompt-recall inject")
	}
	memory.MarkSeen(in.SessionID, hits)
	if out := MergeContext(hookspec.EventUserPromptSubmit, []string{memory.CapChars(b.String(), recallTotalCapChars)}, in.Vendor()); out != "" {
		fmt.Fprintln(stdout, out)
	}
	return ExitProceed
}

// relevantTermHits filters on query coverage, relative score and blocks not already in the session's context.
func relevantTermHits(sessionID string, terms []string, ranked []memory.SearchHit) []memory.SearchHit {
	seenTerms := map[string]bool{}
	for _, t := range terms {
		seenTerms[t] = true
	}
	need := min(max(recallMinMatched, (len(seenTerms)+3)/4), recallMaxMatched)
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

// logDirFor is the access-log dir for a recall hit's bank: local hits log beside the local bank
// (a per-repo audit trail); home and native hits log beside the shared home bank, since that is
// the one access log AccessNominations reads.
func logDirFor(root, bank string) string {
	if bank == "local" {
		return memory.Dir(root)
	}
	return memory.HomeBankDir()
}

// distinctTokenCount gates searches on distinct tokens of at least 3 bytes, without affecting ranking.
func distinctTokenCount(tokens []string) int {
	seen := map[string]bool{}
	for _, t := range tokens {
		if len(t) >= 3 {
			seen[t] = true
		}
	}
	return len(seen)
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
