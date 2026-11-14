package memory

import (
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// recallPicks is the common query preparation and selection for prompt, agent and diagnostic recall.
func recallPicks(ctx context.Context, root, session, query string, semantic, explain bool) ([]recallPick, recallTrace) {
	trace := recallTrace{Silent: true, Embeddings: "not requested", CacheAge: "not inspected"}
	q := textutil.TruncateRunes(strings.TrimSpace(query), recallQueryCapRunes)
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
	ready := startSemantic(ctx, q, semantic)
	seen := seenBlocks(session)
	ranked, files, segs := searchBanksTerms(root, q, terms, 0)
	ranked = followReplaced(ranked, files, segs)
	var sem *Semantic
	semSeen := false
	if semantic {
		res := <-ready
		trace.Degraded = res.err
		ranked, sem, semSeen, trace.Embeddings = recallSemantic(res, files, segs, seen, recallNeed(terms), ranked, tr)
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

func startSemantic(ctx context.Context, query string, enabled bool) <-chan semanticReady {
	ready := make(chan semanticReady, 1)
	if enabled {
		go func() {
			sem, err := embedQuery(ctx, query)
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
	extra, semSeen := sem.candidates(files, segs, recallBodies+recallPointers, recallCosineAdmit, skip)
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

// recallContext renders the prompt recall and returns the delivered hits for bookkeeping.
func recallContext(picks []recallPick) (string, []SearchHit) {
	if len(picks) == 0 {
		return "", nil
	}
	var b strings.Builder
	b.WriteString("<!-- memory-recall; expand: claude-code memory search <id> -->")
	var hits []SearchHit
	for i, p := range picks {
		if i < recallBodies {
			fmt.Fprintf(&b, "\n- [%s/%s] %s", p.Entry.Bank, p.Entry.File, textutil.CapRunes(recallFlat(p.Entry), recallBodyCapRunes, "…"))
		} else {
			fmt.Fprintf(&b, "\n- %s", p.Entry.pointer())
		}
		hits = append(hits, p.Hit)
	}
	return capChars(b.String(), recallTotalCapChars), hits
}

// PromptRecall is the UserPromptSubmit recall block for prompt, "" when nothing clears the floor.
func PromptRecall(ctx context.Context, sessionID, cwd, prompt string, warn io.Writer) string {
	root := SessionRoot(sessionID, cwd)
	picks, trace := recallPicks(ctx, root, sessionID, prompt, true, false)
	if trace.Degraded != nil && os.Getenv("CLAUDE_RECALL_SEMANTIC") != "0" { // the deliberate opt-out is not a degradation
		fmt.Fprintf(warn, "memory-recall: keyword-only (%s)\n", textutil.CapRunes(trace.Degraded.Error(), 120, "…"))
	}
	context, hits := recallContext(picks)
	if context == "" {
		return ""
	}
	logHits(recallRoot(root), hits, accessRecall, time.Now())
	markSeen(sessionID, hits) // bodies mark the key, pointers "p:"+key
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
	return rankFiles(terms, k, files, segs), files, segs
}

// searchAll ranks the home tier plus every repo store as one corpus (`memory search --all`).
func searchAll(query string, k int) []SearchHit {
	files := allCorpusFiles()
	return rankFiles(contentTerms(query), k, files, loadSegments(files, false))
}

func allCorpusFiles() []corpusFile {
	files := corpusFiles(paths.ConfigDir())
	if entries, err := RepoDirs(); err == nil {
		for _, e := range entries {
			if e.Key != harnessStoreKey() {
				files = append(files, storeFiles(e)...)
			}
		}
	}
	return files
}

// SearchRepo ranks blocks for q: every store with all, else the named store or checkout (default: cwd's).
func SearchRepo(repo string, all bool, q string, k int) ([]SearchHit, string, error) {
	if all {
		root := root(repo)
		return searchAll(q, k), root, nil
	}
	top, err := ResolveRepo(repo)
	if err != nil {
		return nil, "", err
	}
	return searchBanks(top, q, k), top, nil
}

// Agent recall previews stay short and the whole block stays under the spec's 1KB cap.
const (
	agentRecallPreviewRunes  = 160
	agentRecallBlockCapBytes = 1024
)

// agentRecallMarker guards against appending twice if a caller already attached one.
const agentRecallMarker = "<memory-recall>"

// AgentRecall is a subagent spawn's <memory-recall> block plus its up to 3 cited ids; ("", nil) on no match.
func AgentRecall(ctx context.Context, sessionID, cwd, task string) (block string, ids []string) {
	cwd = SessionRoot(sessionID, cwd)
	q := strings.TrimSpace(task)
	if q == "" || strings.Contains(q, agentRecallMarker) {
		return "", nil
	}
	picks, _ := recallPicks(ctx, cwd, "", q, false, false) // no seen set: the subagent has not seen what its parent did

	var lines []string
	for _, p := range picks[:min(recallK, len(picks))] {
		lines = append(lines, "\n- "+textutil.CapRunes(p.Entry.pointer(), agentRecallPreviewRunes, "…"))
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

// Recall caps: under 4 distinct terms BM25 ranks noise; the total bounds the per-turn context cost.
const (
	recallMinTerms      = 4
	recallQueryCapRunes = 2000
	recallBodies        = 2 // entries injected whole; the rest ride as one-line pointers
	recallPointers      = 3
	recallK             = 3 // AgentRecall's pointer count
	recallBodyCapRunes  = 240
	recallTotalCapChars = 900
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
		if a.typeRank() != b.typeRank() {
			return a.typeRank() < b.typeRank()
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
func selectRecall(seen map[string]bool, terms []string, ranked []SearchHit, sem *Semantic, semSeen bool, counts map[string]int, trace *recallTrace) []recallPick {
	need := recallNeed(terms)
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
			if trace != nil {
				trace.outcome(blockKey(c.Hit.Block), "below floor")
			}
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
		if key := blockKey(c.Hit.Block); seen[key] || len(out) >= recallBodies && seen["p:"+key] {
			trace.outcome(key, "seen")
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
		if (h.Bank == "archive") != archived || (isSupersededBlock(h.Block) && !h.UnresolvedReplacement) {
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
	cos, ok := sem.cosineHit(c.Hit)
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
			if trace != nil {
				reason := "below floor"
				if h.Matched < need {
					reason = "matched too few"
				}
				trace.outcome(blockKey(h.Block), reason)
			}
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

// seenMaxAge prunes per-session seen sets; a session idle this long has long since compacted or ended.
const seenMaxAge = 7 * 24 * time.Hour

// sessionIDShape matches a Claude Code session id (UUID), rejecting probe/test payloads like "probe-x".
var sessionIDShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// looksLikeSessionID reports whether id has the UUID shape Claude Code assigns real sessions.
func looksLikeSessionID(id string) bool { return sessionIDShape.MatchString(id) }

// blockKey identifies a block by its whitespace-normalized text.
func blockKey(block string) string {
	h := fnv.New64a()
	h.Write([]byte(strings.Join(strings.Fields(block), " ")))
	return strconv.FormatUint(h.Sum64(), 16)
}

func seenPath(sessionID string) string {
	id := filepath.Base(sessionID)
	if id == "" || id == "." || id == ".." || id == string(filepath.Separator) {
		return ""
	}
	return filepath.Join(StateDir(), "recall-seen", id)
}

// resetSeen restarts sessionID's seen set with every corpus block the SessionStart surface carried in full.
func resetSeen(sessionID, root, surface string) {
	if !looksLikeSessionID(sessionID) {
		return
	}
	p := seenPath(sessionID)
	if p == "" {
		return
	}
	pruneSeen(filepath.Dir(p))
	flat := strings.Join(strings.Fields(surface), " ")
	var b strings.Builder
	for _, seg := range loadSegments(corpusFiles(root), true) {
		if seg == nil { // missing or unreadable: the surface could not carry its blocks either
			continue
		}
		for i := range seg.Docs() {
			block, _ := seg.Doc(i)
			if norm := strings.Join(strings.Fields(block), " "); strings.Contains(flat, norm) {
				b.WriteString(blockKey(block))
				b.WriteString("\n")
			}
		}
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = paths.AtomicWrite(p, []byte(b.String()), 0o600)
}

// seenBlocks returns sessionID's seen set: blocks already in the conversation's context.
func seenBlocks(sessionID string) map[string]bool {
	out := map[string]bool{}
	p := seenPath(sessionID)
	if p == "" {
		return out
	}
	data, _ := os.ReadFile(p)
	for k := range strings.FieldsSeq(string(data)) {
		out[k] = true
	}
	return out
}

// markSeen appends the injected hits to sessionID's seen set; fail-open.
func markSeen(sessionID string, hits []SearchHit) {
	p := seenPath(sessionID)
	if p == "" || len(hits) == 0 {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	for i, h := range hits {
		key := blockKey(h.Block)
		if i >= recallBodies { // pointers ride after the bodies; "p:" keeps them promotable to a body later
			key = "p:" + key
		}
		_, _ = f.WriteString(key + "\n")
	}
}

func pruneSeen(dir string) {
	cutoff := time.Now().Add(-seenMaxAge)
	for _, path := range []string{dir, StateDir(), filepath.Dir(compactRecordPath(""))} { // + stale compact records
		ents, _ := os.ReadDir(path)
		for _, e := range ents {
			if !e.Type().IsRegular() || path == StateDir() && !prunableSessionState(e.Name()) && !staleSessionRoot(path, e.Name(), cutoff) {
				continue
			}
			if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(path, e.Name()))
			}
		}
	}
}
