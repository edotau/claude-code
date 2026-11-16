package memory

import (
	"bytes"
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
	return recallBankPicks(ctx, BankFor(recallRoot(root)), session, query, semantic, explain)
}

func recallBankPicks(ctx context.Context, b Bank, session, query string, semantic, explain bool) ([]recallPick, recallTrace) {
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
	ready := startSemantic(ctx, q, semantic)
	seen := seenBlocks(session)
	ranked, files, segs := searchBankTerms(b, q, terms, 0)
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
	counts := returnCounts([]string{accessDir(b), accessDir(HomeBank())}, time.Now())
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

func searchBankTerms(b Bank, query string, terms []string, k int) ([]SearchHit, []corpusFile, []segView) {
	corpus := corpusOf(b)
	files := append(corpus, routedFilesOf(b, query)...)
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
	for _, k := range BankKeys() {
		if k != harnessStoreKey() {
			files = append(files, storeFiles(Bank{Key: k})...)
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
	top, b, err := ResolveBank(repo)
	if err != nil {
		return nil, "", err
	}
	hits, _, _ := searchBankTerms(b, q, contentTerms(q), k)
	return hits, top, nil
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
	h.Write(appendFields(nil, block))
	return strconv.FormatUint(h.Sum64(), 16)
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
	flat := appendFields(nil, surface)
	var norm []byte
	var b strings.Builder
	for _, seg := range loadSegments(corpusFiles(root), true) {
		if seg == nil { // missing or unreadable: the surface could not carry its blocks either
			continue
		}
		for i := range seg.Docs() {
			block, _ := seg.Doc(i)
			if norm = appendFields(norm[:0], block); bytes.Contains(flat, norm) {
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
	// + stale compact records and synthesized agent transcripts
	for _, path := range []string{dir, StateDir(), filepath.Dir(compactRecordPath("")), agentTranscriptDir()} {
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

// RecallRepo is SearchRepo as recall serves it: replaced entries give way to their live replacement, top k kept.
func RecallRepo(repo string, all bool, q string, k int) ([]SearchHit, string, error) {
	if all {
		files := allCorpusFiles()
		segs := loadSegments(files, false)
		hits := followReplaced(rankFiles(contentTerms(q), 0, files, segs), files, segs)
		if k > 0 && len(hits) > k {
			hits = hits[:k]
		}
		return hits, root(repo), nil
	}
	top, b, err := ResolveBank(repo)
	if err != nil {
		return nil, "", err
	}
	ranked, files, segs := searchBankTerms(b, q, contentTerms(q), 0)
	hits := followReplaced(ranked, files, segs)
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits, top, nil
}

// recallCandidate records each selection gate without changing the selection.
type recallCandidate struct {
	ID, File, Tier              string
	BM25                        float64
	Matched, Needed             int
	Cosine                      *float64
	Blended, AccessBoost, Final float64
	Outcome                     string
	key                         string
}

// recallTrace is populated only when explaining a recall.
type recallTrace struct {
	Terms                []string
	Embeddings, CacheAge string
	CacheRows            int
	Silent               bool
	Degraded             error // why semantic recall fell back to keyword-only, nil when it did not
	Rows                 []recallCandidate
}

func (t *recallTrace) outcome(key, outcome string) {
	if t == nil {
		return
	}
	for i := range t.Rows {
		if t.Rows[i].key == key {
			t.Rows[i].Outcome = outcome
		}
	}
}

func (t *recallTrace) candidate(c recallPick, need int, sem *Semantic) {
	if t == nil {
		return
	}
	key := blockKey(c.Hit.Block)
	for _, row := range t.Rows {
		if row.key == key {
			return
		}
	}
	tier := "current"
	if c.Entry.Archived {
		tier = "archive"
	}
	r := recallCandidate{ID: c.Entry.ID, File: c.Entry.Bank + "/" + c.Entry.File, Tier: tier,
		BM25: c.Hit.Score, Matched: c.Hit.Matched, Needed: need, Blended: c.Score,
		Final: c.Score, Outcome: "over cap", key: key}
	if c.Entry.Superseded {
		r.Outcome = "superseded"
	}
	if sem != nil {
		if cos, ok := sem.cosineHit(c.Hit); ok {
			r.Cosine = &cos
		}
	}
	t.Rows = append(t.Rows, r)
}

func (t *recallTrace) scores(c recallPick, boost float64) {
	if t == nil {
		return
	}
	key := blockKey(c.Hit.Block)
	for i := range t.Rows {
		if t.Rows[i].key == key {
			t.Rows[i].Blended = c.Score
			t.Rows[i].AccessBoost = boost
			t.Rows[i].Final = c.Score + boost
		}
	}
}

// recallCacheInfo is diagnostic I/O, never used by the hook's normal path.
func recallCacheInfo(t *recallTrace) {
	t.CacheAge = "missing"
	cache, err := readEmbedCache()
	if err != nil {
		t.CacheAge = "unreadable"
		return
	}
	if cache.Model == "" || len(cache.Rows) == 0 {
		return
	}
	t.CacheRows = len(cache.Rows)
	if cache.MintedAt > 0 {
		t.CacheAge = fmt.Sprint(time.Since(mintedTime(cache)).Round(time.Second))
	} else {
		t.CacheAge = "unminted"
	}
}

func (t *recallTrace) selected(picks []recallPick) {
	if t == nil {
		return
	}
	context, _ := recallContext(picks)
	for i, p := range picks {
		outcome := "over cap"
		line := "- " + p.Entry.pointer()
		if i < recallBodies {
			line = fmt.Sprintf("- [%s/%s] %s", p.Entry.Bank, p.Entry.File, textutil.CapRunes(recallFlat(p.Entry), recallBodyCapRunes, "…"))
		}
		if strings.Contains(context, "\n"+line) {
			outcome = "pointer"
			if i < recallBodies {
				outcome = "body"
			}
		}
		t.outcome(blockKey(p.Hit.Block), outcome)
	}
}

func (t *recallTrace) candidates(ranked []SearchHit, need int, sem *Semantic) {
	for _, archived := range []bool{false, true} {
		_, top := recallTier(ranked, archived, need)
		tierSem := sem
		if archived {
			tierSem = nil
		}
		for _, h := range ranked {
			c := recallTracePick(h)
			if c.Entry.Archived != archived {
				continue
			}
			c.Score, _ = scoreRecall(c, top, false, tierSem)
			t.candidate(c, need, tierSem)
		}
	}
}

func recallTracePick(h SearchHit) recallPick {
	entry := Entry{ID: h.ID, Bank: h.Bank, File: h.File, Archived: h.Bank == "archive",
		Superseded: isSupersededBlock(h.Block)}
	return recallPick{Entry: entry, Hit: h}
}

// recallLoadCap bounds `memory recall --load N`'s total appended bytes across every loaded block.
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
			fmt.Fprintf(errw, "claude-code: memory recall --load: %s: %v\n", h.ID, err)
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
func RecallExplain(w io.Writer, repo, query string) error {
	_, b, err := ResolveBank(repo)
	if err != nil {
		return err
	}
	_, trace := recallBankPicks(context.Background(), b, os.Getenv(SessionIDEnv), query, true, true)
	fmt.Fprintf(w, "query terms: %s\nembeddings: %s\ncache age: %s; rows: %d\nwould stay silent: %t\n",
		strings.Join(trace.Terms, " "), trace.Embeddings, trace.CacheAge, trace.CacheRows, trace.Silent)
	files := explainFiles(w, trace.Rows)
	fmt.Fprintln(w, "id        file tier BM25 match   cosine blend boost final outcome")
	for i, row := range trace.Rows {
		if len(row.ID) > 9 {
			fmt.Fprintf(w, "id @%d: %s\n", i+1, row.ID)
			row.ID = fmt.Sprintf("@%d", i+1)
		}
		explainRow(w, row, files[row.File])
	}
	return nil
}

func explainFiles(w io.Writer, rows []recallCandidate) map[string]int {
	files := make(map[string]int)
	for _, row := range rows {
		if _, ok := files[row.File]; !ok {
			files[row.File] = len(files) + 1
			fmt.Fprintf(w, "file %d: %s\n", files[row.File], row.File)
		}
	}
	fmt.Fprintln(w, "tier: c=current, a=archive; match: matched/needed terms")
	return files
}

func explainRow(w io.Writer, row recallCandidate, file int) {
	cosine := "no vector"
	if row.Cosine != nil {
		cosine = explainNumber(*row.Cosine)
	}
	fmt.Fprintf(w, "%s %d %.1s %s %d/%d %s %s %s %s %s\n",
		row.ID, file, row.Tier, explainNumber(row.BM25), row.Matched, row.Needed,
		cosine, explainNumber(row.Blended), explainNumber(row.AccessBoost),
		explainNumber(row.Final), row.Outcome)
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
