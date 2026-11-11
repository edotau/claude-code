package memory

import (
	"sort"
	"strings"
)

// Recall caps: under 4 distinct terms BM25 ranks noise; the total bounds the per-turn context cost.
const (
	recallMinTerms      = 4
	recallQueryCapRunes = 2000
	recallBodies        = 2 // entries injected whole; the rest ride as one-line pointers
	recallPointers      = 3
	recallK             = 3 // AgentRecall's pointer count
	recallBodyCapRunes  = 380
	recallTotalCapChars = 1500
	recallScoreFloor    = 0.5 // keep hits within half of the top score: BM25 is unnormalised
	// Require several content terms to exclude coincidental overlap.
	recallMinMatched = 2
	recallMaxMatched = 3
	// Blend keyword relevance with normalized cosine, admitting strong semantic matches.
	recallKeywordWeight = 0.5
	recallCosineLow     = 0.45
	recallCosineHigh    = 0.75
	recallCosineAdmit   = 0.62
	recallAccessBoost   = 0.1
	recallAccessCap     = 5
)

// withSemanticCandidates appends cosine-only candidates (zero keyword overlap) the keyword search did not return.
func withSemanticCandidates(ranked, extra []SearchHit) []SearchHit {
	have := map[string]bool{}
	for _, h := range ranked {
		have[blockKey(h.Block)] = true
	}
	for _, h := range extra {
		if !have[blockKey(h.Block)] {
			ranked = append(ranked, h)
		}
	}
	return ranked
}

// recallPick is one surviving entry with its search hit and final score.
type recallPick struct {
	Entry Entry
	Hit   SearchHit
	Score float64
}

// recallFlat is an entry's id and whole body on one line, heading and bullet marks stripped.
func recallFlat(e Entry) string {
	return e.ID + " · " + strings.TrimLeft(strings.Join(strings.Fields(e.Body), " "), "#- ")
}

// orderRecall sorts by final score, using type and date only for exact ties.
func orderRecall(picks []recallPick) {
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].Score != picks[j].Score {
			return picks[i].Score > picks[j].Score
		}
		a, b := picks[i].Entry, picks[j].Entry
		if a.TypeRank() != b.TypeRank() {
			return a.TypeRank() < b.TypeRank()
		}
		return a.Date > b.Date // an undated entry ("") sorts last
	})
}

// recallNeed is how many distinct query terms a hit must contain: a quarter of them, clamped.
func recallNeed(terms []string) int {
	return min(max(recallMinMatched, (distinctCount(terms, 0)+3)/4), recallMaxMatched)
}

// distinctCount counts the distinct tokens of at least minLen bytes.
func distinctCount(tokens []string, minLen int) int {
	seen := map[string]bool{}
	for _, t := range tokens {
		if len(t) >= minLen {
			seen[t] = true
		}
	}
	return len(seen)
}

// selectRecall consults archives only when no current entry passes the relevance gates.
func selectRecall(sessionID string, terms []string, ranked []SearchHit, sem *Semantic, semSeen bool, counts map[string]int, trace *recallTrace) []recallPick {
	need := recallNeed(terms)
	seen := seenBlocks(sessionID)
	if trace != nil {
		trace.candidates(ranked, need, sem)
	}
	for _, archived := range []bool{false, true} {
		tierSem := sem
		if archived {
			tierSem = nil
		}
		picks, passed := pickTier(ranked, archived, need, seen, tierSem, counts, trace)
		if len(picks) > 0 {
			orderRecall(picks)
			trace.selected(picks)
			return picks
		}
		if passed || (semSeen && !archived) {
			return nil
		}
	}
	return nil
}

// pickTier applies the gates before access boosts, then chooses the best final scores.
func pickTier(ranked []SearchHit, archived bool, need int, seen map[string]bool, sem *Semantic, counts map[string]int, trace *recallTrace) ([]recallPick, bool) {
	cands, top := recallTier(ranked, archived, need)
	pool := recallPool(cands, top, need, sem, trace)
	orderRecall(pool)
	passed := len(pool) > 0
	var survivors []recallPick
	for _, c := range pool {
		if c.Score < pool[0].Score*recallScoreFloor {
			trace.outcome(blockKey(c.Hit.Block), "below floor")
			continue
		}
		boost := recallAccessBoost * float64(min(counts[c.Entry.ID], recallAccessCap)) / recallAccessCap
		trace.scores(c, boost)
		c.Score += boost
		survivors = append(survivors, c)
	}
	orderRecall(survivors)
	var out []recallPick
	for _, c := range survivors {
		if seen[blockKey(c.Hit.Block)] {
			trace.outcome(blockKey(c.Hit.Block), "seen")
			continue
		}
		if len(out) < recallBodies+recallPointers {
			out = append(out, c)
		}
	}
	return out, passed
}

func recallTier(ranked []SearchHit, archived bool, need int) ([]SearchHit, float64) {
	var cands []SearchHit
	top, topAny := 0.0, 0.0
	for _, h := range ranked {
		first, _, _ := strings.Cut(h.Block, "\n")
		if (h.Bank == "archive") != archived || strings.Contains(first, "~~") || strings.Contains(first, "SUPERSEDED") {
			continue
		}
		if topAny == 0 {
			topAny = h.Score
		}
		if top == 0 && h.Matched >= need {
			top = h.Score
		}
		cands = append(cands, h)
	}
	if top == 0 {
		top = topAny
	}
	return cands, top
}

func scoreRecall(c recallPick, top float64, admitted bool, sem *Semantic) (float64, bool) {
	kw := 0.0
	if top > 0 {
		kw = min(c.Hit.Score/top, 1)
	}
	if sem == nil {
		return kw, admitted
	}
	cos, ok := sem.CosineHit(c.Hit)
	if !ok {
		return kw, admitted
	} // Entries without a current vector retain their full keyword score.
	blend := recallKeywordWeight*kw + (1-recallKeywordWeight)*min(max((cos-recallCosineLow)/(recallCosineHigh-recallCosineLow), 0), 1)
	return blend, admitted || cos >= recallCosineAdmit
}

func recallPool(cands []SearchHit, top float64, need int, sem *Semantic, trace *recallTrace) []recallPick {
	var pool []recallPick
	for _, h := range cands {
		kwOK := h.Matched >= need && h.Score >= top*recallScoreFloor
		c := recallPick{Hit: h}
		c.Score, kwOK = scoreRecall(c, top, kwOK, sem)
		if !kwOK {
			reason := "below floor"
			if h.Matched < need {
				reason = "matched too few"
			}
			trace.outcome(blockKey(h.Block), reason)
			continue
		}
		c.Entry = entryFromHit(h)
		if h.ID != "" {
			c.Entry.ID = h.ID
		}
		trace.scores(c, 0)
		pool = append(pool, c)
	}
	return pool
}
