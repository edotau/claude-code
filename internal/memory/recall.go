package memory

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// RecallPicks is the common query preparation and selection for prompt, agent and diagnostic recall.
func RecallPicks(root, session, query string, semantic, explain bool) ([]RecallPick, RecallTrace) {
	trace := RecallTrace{Silent: true, Embeddings: "not requested", CacheAge: "not inspected"}
	q := strings.TrimSpace(query)
	if rs := []rune(q); len(rs) > recallQueryCapRunes {
		q = string(rs[:recallQueryCapRunes])
	}
	tokens := textutil.Tokenize(q)
	terms := ContentQueryTerms(tokens)
	var tr *RecallTrace
	if explain {
		tr = &trace
		trace.Terms = terms
		recallCacheInfo(tr)
	}
	if q == "" || (semantic && (strings.HasPrefix(q, "/") || IsUpdateChild())) || distinctTokenCount(tokens) < recallMinTerms {
		trace.Embeddings = "skipped: empty, command, update child or fewer than four distinct terms"
		return nil, trace
	}
	root = recallRoot(root)
	ranked := SearchBanksTerms(root, q, terms, 0)
	var sem *Semantic
	semSeen := false
	if semantic {
		ranked, sem, semSeen, trace.Embeddings = recallSemantic(root, session, q, ranked, tr)
	}
	if len(ranked) == 0 {
		return nil, trace
	}
	counts := ReturnCounts([]string{memstore.Dir(root), memstore.HomeBankDir()}, time.Now())
	picks := selectRecall(session, terms, ranked, sem, semSeen, counts, tr)
	trace.Silent = len(picks) == 0
	return picks, trace
}

func recallRoot(root string) string {
	if root != "" {
		return root
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return paths.ConfigDir()
}

func recallSemantic(root, session, q string, ranked []SearchHit, trace *RecallTrace) ([]SearchHit, *Semantic, bool, string) {
	sem, err := NewSemantic(q)
	if err != nil {
		return ranked, nil, false, "keyword-only: " + err.Error()
	}
	seen := SeenBlocks(session)
	var skipped []SearchHit
	skip := func(h SearchHit) bool {
		if !seen[BlockKey(h.Block)] {
			return false
		}
		if trace != nil {
			skipped = append(skipped, h)
		}
		return true
	}
	extra, semSeen := sem.Candidates(root, recallBodies+recallPointers, recallCosineAdmit, skip)
	if trace != nil {
		have := map[string]bool{}
		for _, h := range ranked {
			have[BlockKey(h.Block)] = true
		}
		for _, h := range skipped {
			if have[BlockKey(h.Block)] {
				continue
			}
			c := RecallPick{Entry: EntryFromHit(h), Hit: h}
			c.Score, _ = scoreRecall(c, 0, false, sem)
			trace.candidate(c, recallNeed(ContentQueryTerms(textutil.Tokenize(q))), sem)
			trace.outcome(BlockKey(h.Block), "seen")
		}
	}
	return withSemanticCandidates(ranked, extra), sem, semSeen, "ran"
}

// RecallContext renders the prompt recall and returns the body and pointer hits for bookkeeping.
func RecallContext(picks []RecallPick) (string, []SearchHit, []SearchHit) {
	if len(picks) == 0 {
		return "", nil, nil
	}
	var b strings.Builder
	b.WriteString("<!-- memory-recall: matching bank entries; expand a pointer: claude-code memory search <id> -->")
	var bodies, hits []SearchHit
	for i, p := range picks {
		if i < recallBodies {
			fmt.Fprintf(&b, "\n- [%s/%s] %s", p.Entry.Bank, p.Entry.File, recallCut(recallFlat(p.Entry), recallBodyCapRunes))
			bodies = append(bodies, p.Hit)
		} else {
			fmt.Fprintf(&b, "\n- %s", p.Entry.Pointer())
		}
		hits = append(hits, p.Hit)
	}
	return CapChars(b.String(), recallTotalCapChars), bodies, hits
}

// RecallPointers supplies the agent adapter's limited pointer list.
func RecallPointers(picks []RecallPick) []Entry {
	var entries []Entry
	for _, p := range picks[:min(recallK, len(picks))] {
		entries = append(entries, p.Entry)
	}
	return entries
}

// RecordRecall logs and marks only the bodies delivered to the prompt.
func RecordRecall(root, session string, bodies, hits []SearchHit) {
	LogHits(recallRoot(root), hits, AccessRecall, time.Now())
	MarkSeen(session, bodies)
}
