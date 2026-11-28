package memory

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/parallel"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// recallPicks is the common query preparation and selection for prompt, agent and diagnostic recall.
func recallPicks(ctx context.Context, session, query string, semantic, explain bool) ([]recallPick, recallTrace) {
	trace := recallTrace{silent: true, embeddings: "not requested", cacheAge: "not inspected"}
	q := textutil.TruncateRunes(strings.TrimSpace(query), recallQueryCapRunes)
	tokens := textutil.Tokenize(q)
	terms := contentQueryTerms(tokens)
	var tr *recallTrace
	if explain {
		tr = &trace
		trace.terms = terms
		recallCacheInfo(tr)
	}
	if skipRecallQuery(q, tokens, semantic) {
		trace.embeddings = "skipped: empty, command, update child or fewer than four distinct terms"
		return nil, trace
	}
	type boosts struct {
		counts  map[string]int
		weights map[string]float64
		err     error
	}
	boosted := make(chan boosts, 1) // the access log and learned weights load while the bank search runs
	parallel.Go(func() { boosted <- boosts{counts: returnCounts(time.Now()), weights: recallWeights()} },
		func(err error) { boosted <- boosts{err: err} })
	seen := seenBlocks(session)
	trace.seen = seen
	ranked, files, segs := searchRecallTerms(terms, 0, true)
	ranked = followReplaced(ranked, files, segs)
	b := <-boosted
	parallel.Rethrow(b.err)
	var sem *semanticQuery
	semSeen := false
	if semantic {
		if !explain && lexicalSufficient(seen, terms, ranked, b.counts, b.weights) {
			trace.embeddings = "skipped: strong lexical hits"
		} else {
			res := <-startSemantic(ctx, q, true)
			trace.degraded = res.err
			ranked, sem, semSeen, trace.embeddings = recallSemantic(res, files, segs, seen, recallNeed(terms), ranked, tr)
		}
	}
	if len(ranked) == 0 {
		return nil, trace
	}
	picks := selectRecall(seen, terms, ranked, sem, semSeen, b.counts, tr, b.weights)
	trace.silent = len(picks) == 0
	return picks, trace
}

// lexicalSufficient reports whether current unseen lexical hits already cover every query term at full strength.
func lexicalSufficient(seen map[string]bool, terms []string, ranked []SearchHit, counts map[string]int, weights map[string]float64) bool {
	if len(terms) == 0 {
		return false
	}
	picks, _ := pickTier(ranked, false, recallNeed(terms), seen, nil, counts, nil, weights)
	if len(picks) == 0 {
		return false
	}
	full := distinctCount(terms, 0)
	bodies := 0
	for _, p := range picks {
		if p.hit.Matched < full {
			return false
		}
		bodies++
	}
	return bodies >= recallBodies || (bodies == 1 && len(picks) == 1)
}

func skipRecallQuery(query string, tokens []string, semantic bool) bool {
	return query == "" || (semantic && (strings.HasPrefix(query, "/") || IsUpdateChild())) || distinctCount(tokens, 3) < recallMinTerms
}

type semanticReady struct {
	sem *semanticQuery
	err error
}

func startSemantic(ctx context.Context, query string, enabled bool) <-chan semanticReady {
	ready := make(chan semanticReady, 1)
	if enabled {
		canon := normalizeQueryVecKey(query) // the embedded text and the cache key share one canonical form
		parallel.Go(func() {
			sem, err := embedQuery(ctx, canon)
			ready <- semanticReady{sem, err}
		}, func(err error) { ready <- semanticReady{err: err} })
	}
	return ready
}

func recallSemantic(result semanticReady, files []corpusFile, segs []segView, seen map[string]bool, need int, ranked []SearchHit, trace *recallTrace) ([]SearchHit, *semanticQuery, bool, string) {
	if result.err != nil {
		return ranked, nil, false, "keyword-only: " + result.err.Error()
	}
	sem := result.sem
	var skipped []SearchHit
	skip := func(h SearchHit) bool {
		if !seen[h.key()] {
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
			have[h.key()] = true
		}
		for _, h := range skipped {
			if have[h.key()] {
				continue
			}
			c := recallTracePick(h)
			c.score, _ = scoreRecall(c, 0, false, sem)
			trace.candidate(c, need, sem)
			trace.outcome(h.key(), "seen")
		}
	}
	return withSemanticCandidates(ranked, extra), sem, semSeen, "ran"
}

// expandKey marks a seen set that already showed the expand header; resetSeen drops it, so a compaction re-shows it.
const expandKey = "expand-shown"

// recallBodyLine is a body hit's line; a native topic whose body opens with its file stem skips the repeated label.
func recallBodyLine(e memoryEntry) string {
	flat := capRecallPreview(recallFlat(e), recallBodyCapRunes)
	if e.typ == "topic" && strings.HasPrefix(strings.TrimLeft(e.body, "#- "), strings.TrimSuffix(e.file, ".md")) {
		return assets.MemoryRecallLine("", flat, false)
	}
	return assets.MemoryRecallLine(e.file, flat, true)
}

func recallPointerLine(e memoryEntry) string {
	return assets.MemoryRecallLine("", capRecallPreview(e.pointer(), recallPointerCapRunes), false)
}

func capRecallPreview(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	end := limit - 1 // reserve one rune for the ellipsis
	for end > 0 && r[end] != ' ' {
		end--
	}
	return string(r[:end]) + "…"
}

// recallContext renders the prompt recall and returns only the hits whose line survived the char cap.
func recallContext(picks []recallPick, expand bool) (string, []SearchHit) {
	if len(picks) == 0 {
		return "", nil
	}
	header := assets.MemoryRecallHeader(expand)
	var b strings.Builder
	b.WriteString(header)
	lines := make([]string, len(picks))
	for i, p := range picks {
		if i < recallBodies {
			lines[i] = "\n" + recallBodyLine(p.entry)
		} else {
			lines[i] = "\n" + recallPointerLine(p.entry)
		}
		b.WriteString(lines[i])
	}
	out := capChars(b.String(), recallTotalCapChars)
	var hits []SearchHit
	for i, p := range picks {
		if !strings.Contains(out, lines[i]) {
			break
		}
		hits = append(hits, p.hit)
	}
	return out, hits
}

// PromptRecall is the UserPromptSubmit recall block for prompt, "" when nothing clears the floor.
func PromptRecall(ctx context.Context, sessionID, prompt string, warn io.Writer) string {
	defer FlushRecallTiming()
	picks, trace := recallPicks(ctx, sessionID, prompt, true, false)
	if trace.degraded != nil && os.Getenv("CLAUDE_RECALL_SEMANTIC") != "0" { // the deliberate opt-out is not a degradation
		fmt.Fprintf(warn, "memory-recall: keyword-only (%s)\n", textutil.CapRunes(trace.degraded.Error(), 120, "…"))
	}
	expand := !trace.seen[expandKey]
	context, hits := recallContext(picks, expand)
	if context == "" {
		return ""
	}
	logHits(hits, accessRecall, time.Now(), sessionID)
	markSeen(sessionID, hits, expand) // bodies mark the key, pointers "p:"+key
	return context
}

const (
	queryVecMax = 200            // cached query vectors kept; the oldest go first
	queryVecAge = 24 * time.Hour // a cached vector older than this is a miss
)

// normalizeQueryVecKey lowercases and collapses whitespace so equivalent queries share one cache entry.
func normalizeQueryVecKey(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(query)), " ")
}

// queryVecFile is the cache file for one (model, query) pair; the "v2" prefix keeps raw-key files from aliasing.
func queryVecFile(model, query string) string {
	sum := sha256.Sum256([]byte("v2\x00" + model + "\x00" + normalizeQueryVecKey(query)))
	return filepath.Join(paths.MemoryStateDir(), "queryvec", hex.EncodeToString(sum[:])+".vec")
}

// readQueryVec is the cached normalized vector for query under model; a missing, stale or malformed file is a miss.
func readQueryVec(model, query string) ([]float32, bool) {
	path := queryVecFile(model, query)
	st, err := os.Stat(path)
	if err != nil || time.Since(st.ModTime()) > queryVecAge {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 || len(b)%4 != 0 {
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	vec := make([]float32, len(b)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return vec, true
}

// writeQueryVec stores a complete vector atomically and trims the cache to queryVecMax files.
func writeQueryVec(model, query string, vec []float32) {
	if len(vec) == 0 {
		return
	}
	path := queryVecFile(model, query)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || paths.AtomicWriteNoSync(path, []byte(encodeVec(vec)), 0o600) != nil {
		return
	}
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(ents) <= queryVecMax {
		return
	}
	mtime := func(e os.DirEntry) time.Time {
		if i, err := e.Info(); err == nil {
			return i.ModTime()
		}
		return time.Time{}
	}
	sort.Slice(ents, func(i, j int) bool { return mtime(ents[i]).Before(mtime(ents[j])) })
	for _, e := range ents[:len(ents)-queryVecMax] {
		_ = os.Remove(filepath.Join(filepath.Dir(path), e.Name()))
	}
}

// cachedQuery answers from a stored query vector, skipping the endpoint lookup, secret read and network call.
func cachedQuery(ctx context.Context, query string) (*semanticQuery, bool) {
	head, err := within(ctx, vectorStore().Head)()
	if err != nil || head.Model == "" || !head.Rows {
		return nil, false
	}
	vec, ok := readQueryVec(head.Model, query)
	if !ok {
		return nil, false
	}
	rows, bad := joinRows(head.Model, len(vec), within(ctx, readEmbedCache))
	if bad.err != nil {
		return nil, false
	}
	return newSemantic(vec, rows), true
}

func recallTiming() bool { return os.Getenv("CLAUDE_RECALL_TIMING") == "1" }

var (
	timingMu    sync.Mutex
	timingOrder []string
	timingSpans = map[string]time.Duration{}
)

func noStage() {}

// RecallStage starts timing a named recall stage and returns its stop; free unless CLAUDE_RECALL_TIMING=1.
func RecallStage(name string) func() {
	if !recallTiming() {
		return noStage
	}
	start := time.Now()
	return func() {
		timingMu.Lock()
		defer timingMu.Unlock()
		if _, seen := timingSpans[name]; !seen {
			timingOrder = append(timingOrder, name)
		}
		timingSpans[name] += time.Since(start)
	}
}

// FlushRecallTiming prints the accumulated stage times to stderr as one line of name=ms pairs.
func FlushRecallTiming() {
	if !recallTiming() {
		return
	}
	timingMu.Lock()
	defer timingMu.Unlock()
	if len(timingOrder) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("recall-timing")
	for _, name := range timingOrder {
		fmt.Fprintf(&b, " %s=%.3f", name, float64(timingSpans[name].Microseconds())/1000)
	}
	fmt.Fprintln(os.Stderr, b.String())
	timingOrder, timingSpans = nil, map[string]time.Duration{}
}

func rankTimed(terms []string, k int, files []corpusFile, segs []segView) []SearchHit {
	defer RecallStage("rank")()
	return rankFiles(terms, k, files, segs)
}

// RowRecall returns store-relative row-rendered bytes of the FTS candidate docs; false declines.
var RowRecall func(terms []string) (docs map[string][]byte, ok bool)

// searchRecallTerms ranks the store for terms, k best; persist lets it write the recall index.
func searchRecallTerms(terms []string, k int, persist bool) ([]SearchHit, []corpusFile, []segView) {
	files := corpusOf()
	segs := loadSegments(files, persist, terms)
	return rankTimed(terms, k, files, segs), files, segs
}

// SearchBank is the one ranking: replaced entries give way to their replacement, k best (0 = all); no index write.
func SearchBank(q string, k int) []SearchHit {
	terms := contentTerms(q)
	if len(terms) == 0 {
		return nil
	}
	ranked, files, segs := searchRecallTerms(terms, 0, false)
	hits := followReplaced(ranked, files, segs)
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

// Agent recall previews stay short and the whole block stays under the spec's 1KB cap.
const (
	agentRecallPreviewRunes  = 160
	agentRecallBlockCapBytes = 1024
)

// AgentRecall is a subagent spawn's <memory-recall> block plus its up to 3 cited ids; ("", nil) on no match.
func AgentRecall(ctx context.Context, sessionID, task string) (block string, ids []string) {
	defer FlushRecallTiming()
	q := strings.TrimSpace(task)
	if q == "" || strings.Contains(q, assets.MemoryRecallMarker()) {
		return "", nil
	}
	picks, _ := recallPicks(ctx, "", q, false, false) // no seen set: the subagent has not seen what its parent did

	var lines []string
	for _, p := range picks[:min(recallK, len(picks))] {
		lines = append(lines, "\n- "+textutil.CapRunes(p.entry.pointer(), agentRecallPreviewRunes, "…"))
		ids = append(ids, p.entry.id)
	}
	block, n := capRecallLines(lines)
	if block == "" {
		return "", nil
	}
	return block, ids[:n]
}

// capRecallLines drops whole trailing lines until the block fits the cap; n is how many lines survived.
func capRecallLines(lines []string) (string, int) {
	marker, footer := assets.MemoryRecallMarker(), assets.MemoryRecallFooter()
	for len(lines) > 0 {
		block := fmt.Sprintf("\n\n%s%s%s", marker, strings.Join(lines, ""), footer)
		if len(block) <= agentRecallBlockCapBytes {
			return block, len(lines)
		}
		lines = lines[:len(lines)-1]
	}
	return "", 0
}

// Recall caps: under 4 distinct terms BM25 ranks noise; the total bounds the per-turn context cost.
const (
	recallMinTerms        = 4
	recallQueryCapRunes   = 2000
	recallBodies          = 2 // entries injected whole; the rest ride as one-line pointers
	recallPointers        = 3
	recallK               = 3 // AgentRecall's pointer count
	recallBodyCapRunes    = 160
	recallPointerCapRunes = 100
	recallTotalCapChars   = 900
	recallScoreFloor      = 0.5 // keep hits within half of the top score: BM25 is unnormalised
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
		have[h.key()] = true
	}
	for _, h := range extra {
		if !have[h.key()] {
			ranked = append(ranked, h)
		}
	}
	return ranked
}

// recallPick is one surviving entry with its search hit and final score.
type recallPick struct {
	entry memoryEntry
	hit   SearchHit
	score float64
}

// recallFlat is an entry's id and whole body on one line, heading and bullet marks stripped.
func recallFlat(e memoryEntry) string {
	return e.id + " · " + strings.TrimLeft(strings.Join(strings.Fields(e.body), " "), "#- ")
}

// orderRecall sorts by final score, using type and date only for exact ties.
func orderRecall(picks []recallPick) {
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].score != picks[j].score {
			return picks[i].score > picks[j].score
		}
		a, b := picks[i].entry, picks[j].entry
		if a.typeRank() != b.typeRank() {
			return a.typeRank() < b.typeRank()
		}
		return a.date > b.date // an undated entry ("") sorts last
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
func selectRecall(seen map[string]bool, terms []string, ranked []SearchHit, sem *semanticQuery, semSeen bool, counts map[string]int, trace *recallTrace, weights map[string]float64) []recallPick {
	need := recallNeed(terms)
	if trace != nil {
		trace.candidates(ranked, need, sem)
	}
	for _, archived := range []bool{false, true} {
		tierSem := sem
		if archived {
			tierSem = nil
		}
		picks, passed := pickTier(ranked, archived, need, seen, tierSem, counts, trace, weights)
		if len(picks) > 0 {
			trace.selected(picks, !seen[expandKey]) // the header the real run prints for this session
			return picks
		}
		if passed || (semSeen && !archived) {
			return nil
		}
	}
	return nil
}

// pickTier applies the gates before access boosts, then chooses the best final scores.
func pickTier(ranked []SearchHit, archived bool, need int, seen map[string]bool, sem *semanticQuery, counts map[string]int, trace *recallTrace, weights map[string]float64) ([]recallPick, bool) {
	cands, top := recallTier(ranked, archived, need)
	pool := recallPool(cands, top, need, sem, trace)
	orderRecall(pool)
	passed := len(pool) > 0
	var survivors []recallPick
	for _, c := range pool {
		if c.score < pool[0].score*recallScoreFloor {
			if trace != nil {
				trace.outcome(c.hit.key(), "below floor")
			}
			continue
		}
		boost := accessBoost(c.entry.id, counts[c.entry.id], weights)
		trace.scores(c, boost)
		c.score += boost
		survivors = append(survivors, c)
	}
	orderRecall(survivors)
	var out []recallPick
	for _, c := range survivors {
		if key := c.hit.key(); seen[key] || len(out) >= recallBodies && seen["p:"+key] {
			trace.outcome(key, "seen")
			continue
		}
		if len(out) < recallBodies+recallPointers {
			out = append(out, c)
		}
	}
	return out, passed
}

func boostFactor(weights map[string]float64, id string) float64 {
	return cmp.Or(weights[id], 1)
}

func accessBoost(id string, count int, weights map[string]float64) float64 {
	boost := recallAccessBoost * float64(min(count, recallAccessCap)) / recallAccessCap
	if boost != 0 {
		boost *= boostFactor(weights, id)
	}
	return boost
}

func recallTier(ranked []SearchHit, archived bool, need int) ([]SearchHit, float64) {
	var cands []SearchHit
	top, topAny := 0.0, 0.0
	for _, h := range ranked {
		if (h.Bank == "archive") != archived || h.Bank == techdebtDir || (isSupersededBlock(h.Block) && !h.UnresolvedReplacement) {
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

func scoreRecall(c recallPick, top float64, admitted bool, sem *semanticQuery) (float64, bool) {
	kw := 0.0
	if top > 0 {
		kw = min(c.hit.Score/top, 1)
	}
	if sem == nil {
		return kw, admitted
	}
	cos, ok := sem.cosineHit(c.hit)
	if !ok {
		return kw, admitted
	} // Entries without a current vector retain their full keyword score.
	blend := recallKeywordWeight*kw + (1-recallKeywordWeight)*min(max((cos-recallCosineLow)/(recallCosineHigh-recallCosineLow), 0), 1)
	return blend, admitted || cos >= recallCosineAdmit
}

func recallPool(cands []SearchHit, top float64, need int, sem *semanticQuery, trace *recallTrace) []recallPick {
	var pool []recallPick
	for _, h := range cands {
		kwOK := h.Matched >= need && h.Score >= top*recallScoreFloor
		c := recallPick{hit: h}
		c.score, kwOK = scoreRecall(c, top, kwOK, sem)
		if !kwOK {
			if trace != nil {
				reason := "below floor"
				if h.Matched < need {
					reason = "matched too few"
				}
				trace.outcome(h.key(), reason)
			}
			continue
		}
		c.entry = entryFromHit(h)
		if h.ID != "" {
			c.entry.id = h.ID
		}
		trace.scores(c, 0)
		pool = append(pool, c)
	}
	return pool
}

// seenMaxAge prunes per-session seen sets; a session idle this long has long since compacted or ended.
const seenMaxAge = 7 * 24 * time.Hour

// sessionIDShape matches a Claude Code session id (UUID), rejecting probe/test payloads like "probe-x".
var sessionIDShape = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
})

// looksLikeSessionID reports whether id has the UUID shape Claude Code assigns real sessions.
func looksLikeSessionID(id string) bool { return sessionIDShape().MatchString(id) }

// blockKey identifies a block by its whitespace-normalized text.
func blockKey(block string) string { return strconv.FormatUint(blockSum(block), 16) }

// blockSum is the FNV-1a sum behind blockKey, kept apart so a segment can store it.
func blockSum(block string) uint64 {
	h := fnv.New64a()
	h.Write(appendFields(nil, block))
	return h.Sum64()
}

// appendFields appends strings.Join(strings.Fields(s), " ") to dst without building the fields slice.
func appendFields(dst []byte, s string) []byte {
	start := len(dst)
	for f := range strings.FieldsSeq(s) {
		if len(dst) > start {
			dst = append(dst, ' ')
		}
		dst = append(dst, f...)
	}
	return dst
}

func seenPath(sessionID string) string {
	id := filepath.Base(sessionID)
	if id == "" || id == "." || id == ".." || id == string(filepath.Separator) {
		return ""
	}
	return filepath.Join(paths.MemoryStateDir(), "recall-seen", id)
}

// resetSeen restarts sessionID's seen set with every corpus block the SessionStart surface carried in full.
func resetSeen(sessionID, surface string) {
	if !looksLikeSessionID(sessionID) {
		return
	}
	p := seenPath(sessionID)
	if p == "" {
		return
	}
	pruned := make(chan struct{})
	parallel.Go(func() { defer close(pruned); pruneSeen(filepath.Dir(p)) }, func(error) {})
	flat := appendFields(nil, surface)
	var norm []byte
	var b strings.Builder
	for _, seg := range loadSegments(corpusOf(), true, nil) {
		if seg == nil { // missing or unreadable: the surface could not carry its blocks either
			continue
		}
		for i := range seg.docs() {
			block, _ := seg.doc(i)
			head, _, _ := strings.Cut(block, "\n") // a block is in flat only if its first line is: skip the full normalise
			if norm = appendFields(norm[:0], head); !bytes.Contains(flat, norm) {
				continue
			}
			if norm = appendFields(norm[:0], block); bytes.Contains(flat, norm) {
				b.WriteString(blockKey(block))
				b.WriteString("\n")
			}
		}
	}
	<-pruned
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = paths.AtomicWriteNoSync(p, []byte(b.String()), 0o600)
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
func markSeen(sessionID string, hits []SearchHit, expanded bool) {
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
		key := h.key()
		if i >= recallBodies { // pointers ride after the bodies; "p:" keeps them promotable to a body later
			key = "p:" + key
		}
		_, _ = f.WriteString(key + "\n")
	}
	if expanded {
		_, _ = f.WriteString(expandKey + "\n")
	}
}

func pruneSeen(dir string) {
	cutoff := time.Now().Add(-seenMaxAge)
	// + stale compact records and synthesized agent transcripts
	for _, path := range []string{dir, paths.MemoryStateDir(), filepath.Dir(compactRecordPath("")), agentTranscriptDir()} {
		ents, _ := os.ReadDir(path)
		for _, e := range ents {
			if !e.Type().IsRegular() || path == paths.MemoryStateDir() && !prunableSessionState(e.Name()) && !staleSessionRoot(path, e.Name(), cutoff) {
				continue
			}
			if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(path, e.Name()))
			}
		}
	}
}

// recallCandidate records each selection gate without changing the selection.
type recallCandidate struct {
	id, file, tier              string
	bm25                        float64
	matched, needed             int
	cosine                      *float64
	blended, accessBoost, final float64
	outcome                     string
	key                         string
}

// recallTrace is populated only when explaining a recall.
type recallTrace struct {
	terms                []string
	embeddings, cacheAge string
	cacheRows            int
	silent               bool
	degraded             error           // why semantic recall fell back to keyword-only, nil when it did not
	seen                 map[string]bool // the session's seen set as recallPicks read it
	rows                 []recallCandidate
}

func (t *recallTrace) outcome(key, outcome string) {
	if t == nil {
		return
	}
	for i := range t.rows {
		if t.rows[i].key == key {
			t.rows[i].outcome = outcome
		}
	}
}

func (t *recallTrace) candidate(c recallPick, need int, sem *semanticQuery) {
	if t == nil {
		return
	}
	key := c.hit.key()
	for _, row := range t.rows {
		if row.key == key {
			return
		}
	}
	tier := "current"
	if c.entry.archived {
		tier = "archive"
	}
	r := recallCandidate{id: c.entry.id, file: c.entry.file, tier: tier,
		bm25: c.hit.Score, matched: c.hit.Matched, needed: need, blended: c.score,
		final: c.score, outcome: "over cap", key: key}
	if c.entry.superseded {
		r.outcome = "superseded"
	}
	if sem != nil {
		if cos, ok := sem.cosineHit(c.hit); ok {
			r.cosine = &cos
		}
	}
	t.rows = append(t.rows, r)
}

func (t *recallTrace) scores(c recallPick, boost float64) {
	if t == nil {
		return
	}
	key := c.hit.key()
	for i := range t.rows {
		if t.rows[i].key == key {
			t.rows[i].blended = c.score
			t.rows[i].accessBoost = boost
			t.rows[i].final = c.score + boost
		}
	}
}

// recallCacheInfo is diagnostic I/O, never used by the hook's normal path.
func recallCacheInfo(t *recallTrace) {
	t.cacheAge = "missing"
	cache, err := readEmbedCache()
	if err != nil {
		t.cacheAge = "unreadable"
		return
	}
	if cache.Model == "" || len(cache.Rows) == 0 {
		return
	}
	t.cacheRows = len(cache.Rows)
	if cache.MintedAt > 0 {
		t.cacheAge = fmt.Sprint(time.Since(mintedTime(cache)).Round(time.Second))
	} else {
		t.cacheAge = "unminted"
	}
}

func (t *recallTrace) selected(picks []recallPick, expand bool) {
	if t == nil {
		return
	}
	context, _ := recallContext(picks, expand)
	for i, p := range picks {
		outcome := "over cap"
		line := recallPointerLine(p.entry)
		if i < recallBodies {
			line = recallBodyLine(p.entry)
		}
		if strings.Contains(context, "\n"+line) {
			outcome = "pointer"
			if i < recallBodies {
				outcome = "body"
			}
		}
		t.outcome(p.hit.key(), outcome)
	}
}

func (t *recallTrace) candidates(ranked []SearchHit, need int, sem *semanticQuery) {
	for _, archived := range []bool{false, true} {
		_, top := recallTier(ranked, archived, need)
		tierSem := sem
		if archived {
			tierSem = nil
		}
		for _, h := range ranked {
			c := recallTracePick(h)
			if c.entry.archived != archived {
				continue
			}
			c.score, _ = scoreRecall(c, top, false, tierSem)
			t.candidate(c, need, tierSem)
		}
	}
}

func recallTracePick(h SearchHit) recallPick {
	entry := memoryEntry{id: h.ID, file: h.File, archived: h.Bank == "archive",
		superseded: isSupersededBlock(h.Block)}
	return recallPick{entry: entry, hit: h}
}

// recallLoadCap bounds `memory search --open N`'s total appended bytes across every loaded block.
const recallLoadCap = 4096

// RecallReport lists the hits as "id — title" and appends the top load bodies; load failures go to errw.
func RecallReport(out, errw io.Writer, hits []SearchHit, load int) {
	if len(hits) == 0 {
		fmt.Fprintln(out, "no matching blocks (banks empty or query all stopwords)")
		return
	}
	for _, h := range hits {
		fmt.Fprintf(out, "%s — %s\n", h.ID, h.Title())
	}
	if load > 0 {
		recallLoad(out, errw, hits, load)
	}
}

// recallLoad appends the top n hits' full block text (ShowBlock), capped at recallLoadCap bytes written in total.
func recallLoad(out, errw io.Writer, hits []SearchHit, n int) {
	if n > len(hits) {
		n = len(hits)
	}
	budget := recallLoadCap
	for _, h := range hits[:n] {
		_, block, err := ShowBlock(h.ID)
		if err != nil {
			fmt.Fprintf(errw, "claude-code: memory search --open: %s: %v\n", h.ID, err)
			continue
		}
		block = strings.TrimSpace(block)
		wrapper := len(fmt.Sprintf("\n<!-- %s -->\n\n", h.ID))
		if wrapper+len(block) > budget {
			if budget < recallLoadCap {
				continue // only the first block is cut; a later one that does not fit whole is skipped, never a fragment
			}
			block = textutil.CutBytes(block, budget-wrapper)
		}
		fmt.Fprintf(out, "\n<!-- %s -->\n%s\n", h.ID, block)
		budget -= wrapper + len(block)
	}
}

// RecallExplain writes the hook-recall selection trace for query: terms, cache state and every candidate's scores.
func RecallExplain(w io.Writer, query string) {
	_, trace := recallPicks(context.Background(), os.Getenv(paths.SessionIDEnv), query, true, true)
	fmt.Fprintf(w, "query terms: %s\nembeddings: %s\ncache age: %s; rows: %d\nwould stay silent: %t\n",
		strings.Join(trace.terms, " "), trace.embeddings, trace.cacheAge, trace.cacheRows, trace.silent)
	files := explainFiles(w, trace.rows)
	fmt.Fprintln(w, "id        file tier BM25 match   cosine blend boost final outcome")
	for i, row := range trace.rows {
		if len(row.id) > 9 {
			fmt.Fprintf(w, "id @%d: %s\n", i+1, row.id)
			row.id = fmt.Sprintf("@%d", i+1)
		}
		explainRow(w, row, files[row.file])
	}
}

func explainFiles(w io.Writer, rows []recallCandidate) map[string]int {
	files := make(map[string]int)
	for _, row := range rows {
		if _, ok := files[row.file]; !ok {
			files[row.file] = len(files) + 1
			fmt.Fprintf(w, "file %d: %s\n", files[row.file], row.file)
		}
	}
	fmt.Fprintln(w, "tier: c=current, a=archive; match: matched/needed terms")
	return files
}

func explainRow(w io.Writer, row recallCandidate, file int) {
	cosine := "no vector"
	if row.cosine != nil {
		cosine = explainNumber(*row.cosine)
	}
	fmt.Fprintf(w, "%s %d %.1s %s %d/%d %s %s %s %s %s\n",
		row.id, file, row.tier, explainNumber(row.bm25), row.matched, row.needed,
		cosine, explainNumber(row.blended), explainNumber(row.accessBoost),
		explainNumber(row.final), row.outcome)
}

func explainNumber(value float64) string {
	s := fmt.Sprintf("%.2g", value)
	if len(s) > 6 {
		s = fmt.Sprintf("%.1g", value)
	}
	s = strings.ReplaceAll(s, "e+", "e")
	s = strings.ReplaceAll(s, "e-0", "e-")
	return strings.ReplaceAll(s, "e0", "e")
}
