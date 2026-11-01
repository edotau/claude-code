package recall

import (
	"fmt"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memory/recall/embed"
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
	terms := bank.ContentQueryTerms(tokens)
	var tr *RecallTrace
	if explain {
		tr = &trace
		trace.Terms = terms
		recallCacheInfo(tr)
	}
	if q == "" || (semantic && (strings.HasPrefix(q, "/") || bank.IsUpdateChild())) || distinctTokenCount(tokens) < recallMinTerms {
		trace.Embeddings = "skipped: empty, command, update child or fewer than four distinct terms"
		return nil, trace
	}
	root = recallRoot(root)
	ready := startSemantic(q, semantic)
	ranked, files, segs := searchBanksTerms(root, q, terms, 0)
	var sem *embed.Semantic
	semSeen := false
	if semantic {
		ranked, sem, semSeen, trace.Embeddings = recallSemantic(<-ready, files, segs, session, recallNeed(terms), ranked, tr)
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

type semanticReady struct {
	sem *embed.Semantic
	err error
}

func startSemantic(query string, enabled bool) <-chan semanticReady {
	ready := make(chan semanticReady, 1)
	if enabled {
		go func() {
			sem, err := embed.NewSemantic(query)
			ready <- semanticReady{sem, err}
		}()
	}
	return ready
}

func recallSemantic(result semanticReady, files []bank.CorpusFile, segs []bank.SegView, session string, need int, ranked []bank.SearchHit, trace *RecallTrace) ([]bank.SearchHit, *embed.Semantic, bool, string) {
	if result.err != nil {
		return ranked, nil, false, "keyword-only: " + result.err.Error()
	}
	sem := result.sem
	seen := SeenBlocks(session)
	var skipped []bank.SearchHit
	skip := func(h bank.SearchHit) bool {
		if !seen[BlockKey(h.Block)] {
			return false
		}
		if trace != nil {
			skipped = append(skipped, h)
		}
		return true
	}
	extra, semSeen := sem.Candidates(files, segs, recallBodies+recallPointers, recallCosineAdmit, skip)
	if trace != nil {
		have := map[string]bool{}
		for _, h := range ranked {
			have[BlockKey(h.Block)] = true
		}
		for _, h := range skipped {
			if have[BlockKey(h.Block)] {
				continue
			}
			c := recallTracePick(h)
			c.Score, _ = scoreRecall(c, 0, false, sem)
			trace.candidate(c, need, sem)
			trace.outcome(BlockKey(h.Block), "seen")
		}
	}
	return withSemanticCandidates(ranked, extra), sem, semSeen, "ran"
}

// RecallContext renders the prompt recall and returns the body and pointer hits for bookkeeping.
func RecallContext(picks []RecallPick) (string, []bank.SearchHit, []bank.SearchHit) {
	if len(picks) == 0 {
		return "", nil, nil
	}
	var b strings.Builder
	b.WriteString("<!-- memory-recall: matching bank entries; expand a pointer: claude-code memory search <id> -->")
	var bodies, hits []bank.SearchHit
	for i, p := range picks {
		if i < recallBodies {
			fmt.Fprintf(&b, "\n- [%s/%s] %s", p.Entry.Bank, p.Entry.File, recallCut(recallFlat(p.Entry), recallBodyCapRunes))
			bodies = append(bodies, p.Hit)
		} else {
			fmt.Fprintf(&b, "\n- %s", p.Entry.Pointer())
		}
		hits = append(hits, p.Hit)
	}
	return bank.CapChars(b.String(), recallTotalCapChars), bodies, hits
}

// RecallPointers supplies the agent adapter's limited pointer list.
func RecallPointers(picks []RecallPick) []bank.Entry {
	var entries []bank.Entry
	for _, p := range picks[:min(recallK, len(picks))] {
		entries = append(entries, p.Entry)
	}
	return entries
}

// RecordRecall logs and marks only the bodies delivered to the prompt.
func RecordRecall(root, session string, bodies, hits []bank.SearchHit) {
	LogHits(recallRoot(root), hits, AccessRecall, time.Now())
	MarkSeen(session, bodies)
}
