package memory

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

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
		if cos, ok := sem.CosineHit(c.Hit); ok {
			r.Cosine = &cos
		}
	}
	t.Rows = append(t.Rows, r)
}

func (t *recallTrace) scores(c recallPick, boost float64) {
	if t == nil {
		return
	}
	for i := range t.Rows {
		if t.Rows[i].key == blockKey(c.Hit.Block) {
			t.Rows[i].Blended = c.Score
			t.Rows[i].AccessBoost = boost
			t.Rows[i].Final = c.Score + boost
		}
	}
}

// recallCacheInfo is diagnostic I/O, never used by the hook's normal path.
func recallCacheInfo(t *recallTrace) {
	t.CacheAge = "missing"
	for _, name := range []string{providers.SessionProfile(), providers.AgentRecord("claude").Profile} {
		if name == "" || !safeProfile.MatchString(name) {
			continue
		}
		cache, err := readProfileCache(name)
		if err != nil {
			t.CacheAge = "unreadable"
			continue
		}
		if cache.Model == "" || len(cache.Rows) == 0 {
			continue
		}
		t.CacheRows = len(cache.Rows)
		if cache.MintedAt > 0 {
			t.CacheAge = fmt.Sprint(time.Since(mintedTime(cache)).Round(time.Second))
		} else {
			t.CacheAge = "unminted"
		}
		return
	}
}

func (t *recallTrace) selected(picks []recallPick) {
	if t == nil {
		return
	}
	context, _, _ := RecallContext(picks)
	for i, p := range picks {
		outcome := "over cap"
		line := "- " + p.Entry.Pointer()
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
	first, _, _ := strings.Cut(h.Block, "\n")
	entry := Entry{ID: h.ID, Bank: h.Bank, File: h.File, Archived: h.Bank == "archive",
		Superseded: isSupersededTitle(first)}
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
			cut := budget - wrapper
			for cut > 0 && !utf8.RuneStart(block[cut]) {
				cut--
			}
			block = block[:cut]
		}
		fmt.Fprintf(out, "\n<!-- %s -->\n%s\n", h.ID, block)
		budget -= wrapper + len(block)
	}
}

// RecallExplain writes the hook-recall selection trace for query: terms, cache state and every candidate's scores.
func RecallExplain(w io.Writer, repo, query string) {
	_, trace := RecallPicks(RepoRoot(repo), os.Getenv(SessionIDEnv), query, true, true)
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
