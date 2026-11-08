package recall

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memory/recall/embed"

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
	if skipRecallQuery(q, tokens, semantic) {
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

func skipRecallQuery(query string, tokens []string, semantic bool) bool {
	return query == "" || (semantic && (strings.HasPrefix(query, "/") || bank.IsUpdateChild())) || distinctTokenCount(tokens) < recallMinTerms
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
			fmt.Fprintf(&b, "\n- [%s/%s] %s", p.Entry.Bank, p.Entry.File, Cut(recallFlat(p.Entry), recallBodyCapRunes))
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

// SearchBanks ranks every block of root's local, home and archive banks plus native topics; top k (0 = all).
func SearchBanks(root, query string, k int) []bank.SearchHit {
	return SearchBanksTerms(root, query, bank.ContentTerms(query), k)
}

// SearchBanksTerms ranks banks using the caller's pretokenized content terms.
func SearchBanksTerms(root, query string, terms []string, k int) []bank.SearchHit {
	ranked, _, _ := searchBanksTerms(root, query, terms, k)
	return ranked
}

func searchBanksTerms(root, query string, terms []string, k int) ([]bank.SearchHit, []bank.CorpusFile, []bank.SegView) {
	corpus := bank.CorpusFiles(root)
	files := append(corpus, bank.RoutedFiles(root, query)...)
	segs := loadSegments(files, true)
	return bank.RankFiles(terms, k, files, segs), files[:len(corpus)], segs[:len(corpus)]
}

// SearchAll ranks the home tier plus every repo store as one corpus (`memory search --all`).
func SearchAll(query string, k int) []bank.SearchHit {
	files := bank.CorpusFiles(paths.ConfigDir())
	if entries, err := memstore.RepoDirs(); err == nil {
		for _, e := range entries {
			if e.Key != memstore.HarnessStoreKey() {
				files = append(files, bank.StoreFiles(e)...)
			}
		}
	}
	return bank.RankFiles(bank.ContentTerms(query), k, files, loadSegments(files, false))
}

// SearchRepo ranks blocks for q: every store with all, else the named store or checkout (default: cwd's).
func SearchRepo(repo string, all bool, q string, k int) ([]bank.SearchHit, string) {
	root := memstore.Root(repo)
	switch {
	case all:
		return SearchAll(q, k), root
	case repo != "": // a store name from the catalog, else a checkout path
		if e, ok := memstore.RepoByName(repo); ok && e.Origin.Origin != "" {
			root = e.Origin.Origin
		}
	}
	return SearchBanks(root, q, k), root
}
