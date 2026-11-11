package memory

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// recallPicks is the common query preparation and selection for prompt, agent and diagnostic recall.
func recallPicks(root, session, query string, semantic, explain bool) ([]recallPick, recallTrace) {
	trace := recallTrace{Silent: true, Embeddings: "not requested", CacheAge: "not inspected"}
	q := strings.TrimSpace(query)
	if rs := []rune(q); len(rs) > recallQueryCapRunes {
		q = string(rs[:recallQueryCapRunes])
	}
	tokens := textutil.Tokenize(q)
	terms := contentQueryTerms(tokens)
	var tr *recallTrace
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
	seen := seenBlocks(session)
	ranked, files, segs := searchBanksTerms(root, q, terms, 0)
	var sem *Semantic
	semSeen := false
	if semantic {
		ranked, sem, semSeen, trace.Embeddings = recallSemantic(<-ready, files, segs, seen, recallNeed(terms), ranked, tr)
	}
	if len(ranked) == 0 {
		return nil, trace
	}
	counts := returnCounts([]string{Dir(root), HomeBankDir()}, time.Now())
	picks := selectRecall(seen, terms, ranked, sem, semSeen, counts, tr)
	trace.Silent = len(picks) == 0
	return picks, trace
}

func skipRecallQuery(query string, tokens []string, semantic bool) bool {
	return query == "" || (semantic && (strings.HasPrefix(query, "/") || IsUpdateChild())) || distinctCount(tokens, 3) < recallMinTerms
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
	sem *Semantic
	err error
}

func startSemantic(query string, enabled bool) <-chan semanticReady {
	ready := make(chan semanticReady, 1)
	if enabled {
		go func() {
			sem, err := NewSemantic(query)
			ready <- semanticReady{sem, err}
		}()
	}
	return ready
}

func recallSemantic(result semanticReady, files []corpusFile, segs []segView, seen map[string]bool, need int, ranked []SearchHit, trace *recallTrace) ([]SearchHit, *Semantic, bool, string) {
	if result.err != nil {
		return ranked, nil, false, "keyword-only: " + result.err.Error()
	}
	sem := result.sem
	var skipped []SearchHit
	skip := func(h SearchHit) bool {
		if !seen[blockKey(h.Block)] {
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
			have[blockKey(h.Block)] = true
		}
		for _, h := range skipped {
			if have[blockKey(h.Block)] {
				continue
			}
			c := recallTracePick(h)
			c.Score, _ = scoreRecall(c, 0, false, sem)
			trace.candidate(c, need, sem)
			trace.outcome(blockKey(h.Block), "seen")
		}
	}
	return withSemanticCandidates(ranked, extra), sem, semSeen, "ran"
}

// recallContext renders the prompt recall and returns the body and pointer hits for bookkeeping.
func recallContext(picks []recallPick) (string, []SearchHit, []SearchHit) {
	if len(picks) == 0 {
		return "", nil, nil
	}
	var b strings.Builder
	b.WriteString("<!-- memory-recall: matching bank entries; expand a pointer: claude-code memory search <id> -->")
	var bodies, hits []SearchHit
	for i, p := range picks {
		if i < recallBodies {
			fmt.Fprintf(&b, "\n- [%s/%s] %s", p.Entry.Bank, p.Entry.File, textutil.CapRunes(recallFlat(p.Entry), recallBodyCapRunes, "…"))
			bodies = append(bodies, p.Hit)
		} else {
			fmt.Fprintf(&b, "\n- %s", p.Entry.Pointer())
		}
		hits = append(hits, p.Hit)
	}
	return capChars(b.String(), recallTotalCapChars), bodies, hits
}

// PromptRecall is the UserPromptSubmit recall block for prompt, "" when nothing clears the floor.
func PromptRecall(sessionID, cwd, prompt string, warn io.Writer) string {
	root := SessionRoot(sessionID, cwd)
	picks, trace := recallPicks(root, sessionID, prompt, true, false)
	after, ok := strings.CutPrefix(trace.Embeddings, "keyword-only: ")
	if ok && os.Getenv("CLAUDE_RECALL_SEMANTIC") != "0" { // the deliberate opt-out is not a degradation
		fmt.Fprintf(warn, "memory-recall: keyword-only (%s)\n", textutil.CapRunes(after, 120, "…"))
	}
	context, bodies, hits := recallContext(picks)
	if context == "" {
		return ""
	}
	logHits(recallRoot(root), hits, accessRecall, time.Now())
	markSeen(sessionID, bodies) // only the bodies delivered count as seen
	return context
}

// searchBanks ranks every block of root's local, home and archive banks plus native topics; top k (0 = all).
func searchBanks(root, query string, k int) []SearchHit {
	ranked, _, _ := searchBanksTerms(root, query, contentTerms(query), k)
	return ranked
}

func searchBanksTerms(root, query string, terms []string, k int) ([]SearchHit, []corpusFile, []segView) {
	corpus := corpusFiles(root)
	files := append(corpus, routedFiles(root, query)...)
	segs := loadSegments(files, true)
	return rankFiles(terms, k, files, segs), files[:len(corpus)], segs[:len(corpus)]
}

// searchAll ranks the home tier plus every repo store as one corpus (`memory search --all`).
func searchAll(query string, k int) []SearchHit {
	files := corpusFiles(paths.ConfigDir())
	if entries, err := RepoDirs(); err == nil {
		for _, e := range entries {
			if e.Key != harnessStoreKey() {
				files = append(files, storeFiles(e)...)
			}
		}
	}
	return rankFiles(contentTerms(query), k, files, loadSegments(files, false))
}

// SearchRepo ranks blocks for q: every store with all, else the named store or checkout (default: cwd's).
func SearchRepo(repo string, all bool, q string, k int) ([]SearchHit, string) {
	if all {
		root := root(repo)
		return searchAll(q, k), root
	}
	root := RepoRoot(repo) // a store name from the catalog, else a checkout path
	return searchBanks(root, q, k), root
}

// Agent recall previews stay short and the whole block stays under the spec's 1KB cap.
const (
	agentRecallPreviewRunes  = 160
	agentRecallBlockCapBytes = 1024
)

// agentRecallMarker guards against appending twice if a caller already attached one.
const agentRecallMarker = "<memory-recall>"

// AgentRecall is a subagent spawn's <memory-recall> block plus its up to 3 cited ids; ("", nil) on no match.
func AgentRecall(sessionID, cwd, task string) (block string, ids []string) {
	cwd = SessionRoot(sessionID, cwd)
	q := strings.TrimSpace(task)
	if q == "" || strings.Contains(q, agentRecallMarker) {
		return "", nil
	}
	picks, _ := recallPicks(cwd, sessionID, q, false, false)

	var lines []string
	for _, p := range picks[:min(recallK, len(picks))] {
		lines = append(lines, "\n- "+textutil.CapRunes(p.Entry.Pointer(), agentRecallPreviewRunes, "…"))
		ids = append(ids, p.Entry.ID)
	}
	block, n := capRecallLines(lines)
	if block == "" {
		return "", nil
	}
	return block, ids[:n]
}

// capRecallLines drops whole trailing lines until the block fits the cap; n is how many lines survived.
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
