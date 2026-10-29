package hooks

import (
	"fmt"
	"io"
	"os"
	"sort"
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
	recallBodies        = 2 // entries injected whole; the rest ride as one-line pointers
	recallPointers      = 3
	recallK             = 3 // agentRecall's pointer count
	recallBodyCapRunes  = 380
	recallTotalCapChars = 1500
	recallScoreFloor    = 0.5 // keep hits within half of the top score: BM25 is unnormalised
	// A hit must match a quarter of the query's content terms, clamped: one shared word is coincidence.
	recallMinMatched = 2
	recallMaxMatched = 3
	// Blend: keyword share of the final score, the cosine span mapped onto 0..1, and the cosine that admits an
	// entry the keyword gate rejected (measured on gte-large-en: bank median 0.47, relevant 0.63-0.74, unrelated <0.5).
	recallKeywordWeight = 0.5
	recallCosineLow     = 0.45
	recallCosineHigh    = 0.75
	recallCosineAdmit   = 0.62
)

// MemoryRecall (UserPromptSubmit) injects the best memory-bank entries for the prompt: two whole, the rest as
// pointers; silent on a slash command, a short prompt, or no match.
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
	ranked := memory.SearchBanksTerms(root, q, terms, 0)
	sem, err := memory.NewSemantic(q)
	semSeen := false // a current semantic match is already in context, so the archive must stay out
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory-recall: keyword-only (%s)\n", recallCut(err.Error(), 120))
	} else {
		seen := memory.SeenBlocks(in.SessionID)
		skip := func(h memory.SearchHit) bool { return seen[memory.BlockKey(h.Block)] } // before top-k, or seen ones crowd it
		var extra []memory.SearchHit
		extra, semSeen = sem.Candidates(root, recallBodies+recallPointers, recallCosineAdmit, skip)
		ranked = withSemanticCandidates(ranked, extra)
	}
	if len(ranked) == 0 {
		return ExitProceed
	}
	picks := selectRecall(in.SessionID, terms, ranked, sem, semSeen)
	if len(picks) == 0 {
		return ExitProceed
	}
	var b strings.Builder
	b.WriteString("<!-- memory-recall: matching bank entries; expand a pointer: claude-code memory search <id> -->")
	now := time.Now()
	access := map[string][]string{}
	var bodies []memory.SearchHit
	for i, p := range picks {
		if i < recallBodies {
			fmt.Fprintf(&b, "\n- [%s/%s] %s", p.e.Bank, p.e.File, recallCut(recallFlat(p.e), recallBodyCapRunes))
			bodies = append(bodies, p.hit)
		} else {
			fmt.Fprintf(&b, "\n- %s", p.e.Pointer())
		}
		dir := logDirFor(root, p.e.Bank)
		access[dir] = append(access[dir], p.e.Bank+":"+p.e.File)
	}
	for dir, entries := range access {
		memory.LogAccessBatch(dir, now, entries, "prompt-recall inject")
	}
	memory.MarkSeen(in.SessionID, bodies) // a pointer is not in context yet, so it may still earn a body later
	if out := MergeContext(hookspec.EventUserPromptSubmit, []string{memory.CapChars(b.String(), recallTotalCapChars)}, in.Vendor()); out != "" {
		fmt.Fprintln(stdout, out)
	}
	return ExitProceed
}

// withSemanticCandidates appends cosine-only candidates (zero keyword overlap) the keyword search did not return.
func withSemanticCandidates(ranked, extra []memory.SearchHit) []memory.SearchHit {
	have := map[string]bool{}
	for _, h := range ranked {
		have[memory.BlockKey(h.Block)] = true
	}
	for _, h := range extra {
		if !have[memory.BlockKey(h.Block)] {
			ranked = append(ranked, h)
		}
	}
	return ranked
}

// recallPick is one surviving entry with its search hit and final score.
type recallPick struct {
	e     memory.Entry
	hit   memory.SearchHit
	score float64
}

// recallFlat is an entry's id and whole body on one line, heading and bullet marks stripped.
func recallFlat(e memory.Entry) string {
	return e.ID + " · " + strings.TrimLeft(strings.Join(strings.Fields(e.Body), " "), "#- ")
}

// selectRecall filters, scores and orders the ranked hits: superseded and already-seen entries drop, archive is
// consulted only when no current entry clears the gate, and the survivors come back decisions first, newest first.
func selectRecall(sessionID string, terms []string, ranked []memory.SearchHit, sem *memory.Semantic, semSeen bool) []recallPick {
	need := recallNeed(terms)
	seen := memory.SeenBlocks(sessionID)
	for _, archived := range []bool{false, true} {
		tierSem := sem
		if archived {
			tierSem = nil // archive entries carry no vectors
		}
		picks, passed := pickTier(ranked, archived, need, seen, tierSem)
		if len(picks) > 0 {
			orderRecall(picks)
			return picks
		}
		if passed || (semSeen && !archived) { // the current answer exists but is already in context: never fall back to an older one
			return nil
		}
	}
	return nil
}

// pickTier scores one tier (current or archive): keyword gate, or the semantic admit, then the relative floor.
func pickTier(ranked []memory.SearchHit, archived bool, need int, seen map[string]bool, sem *memory.Semantic) (out []recallPick, passed bool) {
	var cands []recallPick
	top, topAny := 0.0, 0.0
	for _, h := range ranked {
		e := memory.EntryFromHit(h)
		if e.Archived != archived || e.Superseded {
			continue
		}
		if topAny == 0 {
			topAny = h.Score
		}
		if top == 0 && h.Matched >= need {
			top = h.Score
		}
		cands = append(cands, recallPick{e: e, hit: h})
	}
	if top == 0 {
		top = topAny
	}
	var pool []recallPick
	for _, c := range cands {
		kwOK := c.hit.Matched >= need && c.hit.Score >= top*recallScoreFloor
		kw := 0.0
		if top > 0 {
			kw = c.hit.Score / top
		}
		c.score = kw
		if sem != nil {
			cos, ok := sem.Cosine(c.e)
			if !ok { // no vector (edited since the index): its keyword share carries the same weight, so scores compare
				c.score = recallKeywordWeight * kw
			} else {
				c.score = recallKeywordWeight*kw + (1-recallKeywordWeight)*min(max((cos-recallCosineLow)/(recallCosineHigh-recallCosineLow), 0), 1)
				kwOK = kwOK || cos >= recallCosineAdmit
			}
		}
		if kwOK {
			pool = append(pool, c)
		}
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].score > pool[j].score })
	passed = len(pool) > 0 // pool[0] always clears its own relative floor
	for _, c := range pool {
		if c.score < pool[0].score*recallScoreFloor || len(out) == recallBodies+recallPointers {
			break
		}
		if !seen[memory.BlockKey(c.hit.Block)] {
			out = append(out, c)
		}
	}
	return out, passed
}

// orderRecall sorts decisions and conventions ahead of other types and session narrative, newest first within a rank.
func orderRecall(picks []recallPick) {
	sort.SliceStable(picks, func(i, j int) bool {
		a, b := picks[i].e, picks[j].e
		if a.TypeRank() != b.TypeRank() {
			return a.TypeRank() < b.TypeRank()
		}
		return a.Date > b.Date // an undated entry ("") sorts last
	})
}

// recallNeed is how many distinct query terms a hit must contain: a quarter of them, clamped.
func recallNeed(terms []string) int {
	uniq := map[string]bool{}
	for _, t := range terms {
		uniq[t] = true
	}
	return min(max(recallMinMatched, (len(uniq)+3)/4), recallMaxMatched)
}

// relevantTermHits filters on query coverage, relative score and blocks not already in the session's context;
// the subagent pointer block (agentRecall) ranks with it.
func relevantTermHits(sessionID string, terms []string, ranked []memory.SearchHit) []memory.SearchHit {
	need := recallNeed(terms)
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
