package memory

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

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

// recallLoad appends the top n hits' full block text (ShowBlock), capped at recallLoadCap bytes total.
func recallLoad(out, errw io.Writer, hits []SearchHit, n int) {
	if n > len(hits) {
		n = len(hits)
	}
	budget := recallLoadCap
	for _, h := range hits[:n] {
		if budget <= 0 {
			return
		}
		_, block, err := ShowBlock(h.ID)
		if err != nil {
			fmt.Fprintf(errw, "claude-code: memory recall --load: %s: %v\n", h.ID, err)
			continue
		}
		block = strings.TrimSpace(block)
		if len(block) > budget {
			if budget < recallLoadCap {
				return // only the first block is cut: its rune-boundary cut leaves 1-3 bytes, a fragment
			}
			cut := budget
			for cut > 0 && !utf8.RuneStart(block[cut]) {
				cut--
			}
			block = block[:cut]
		}
		fmt.Fprintf(out, "\n<!-- %s -->\n%s\n", h.ID, block)
		budget -= len(block)
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
