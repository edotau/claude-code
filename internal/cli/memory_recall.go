package cli

import (
	"fmt"
	"github.com/edotau/claude-code/internal/memory/recall"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// recallLoadCap bounds `memory recall --load N`'s total appended bytes across every loaded block.
const recallLoadCap = 4096

func cmdMemoryRecall(args []string) int {
	fs, repo := memoryRepoFlagSet("recall")
	k := fs.Int("k", 3, "maximum hits")
	all := fs.Bool("all", false, "search the home tier and every repo store")
	load := fs.Int("load", 0, "append the show bodies of the top N hits (4KB cap)")
	explain := fs.Bool("explain", false, "explain hook recall selection and candidate scores")
	format := helptext.FormatFlag(fs)
	terms, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("memory recall", err)
	}
	if len(terms) == 0 {
		return fail("memory recall: no query (usage: %s)", memorySubPage("recall").Usage)
	}
	if *explain {
		if *all || *format == helptext.JSON || *load != 0 {
			return fail("memory recall: --explain does not support --all, --format json, or --load")
		}
		return recallExplain(*repo, strings.Join(terms, " "))
	}
	hits, _ := searchHits(*repo, *all, strings.Join(terms, " "), *k)
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, hits); err != nil {
			return fail("memory recall: %v", err)
		}
		return 0
	}
	return recallPrint(hits, *load)
}

func recallPrint(hits []bank.SearchHit, load int) int {
	if len(hits) == 0 {
		fmt.Println("no matching blocks (banks empty or query all stopwords)")
		return 0
	}
	for _, h := range hits {
		fmt.Printf("%s — %s\n", h.ID, h.Title())
	}
	if load > 0 {
		recallLoad(hits, load)
	}
	return 0
}

// recallLoad appends the top n hits' full block text (ShowBlock), capped at recallLoadCap bytes total.
func recallLoad(hits []bank.SearchHit, n int) {
	if n > len(hits) {
		n = len(hits)
	}
	budget := recallLoadCap
	for _, h := range hits[:n] {
		if budget <= 0 {
			continue
		}
		_, block, err := bank.ShowBlock(h.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "claude-code: memory recall --load: %s: %v\n", h.ID, err)
			continue
		}
		block = strings.TrimSpace(block)
		if len(block) > budget {
			block = block[:budget]
		}
		fmt.Printf("\n<!-- %s -->\n%s\n", h.ID, block)
		budget -= len(block)
	}
}

func recallExplain(repo, query string) int {
	root := memstore.Root(repo)
	if e, ok := memstore.RepoByName(repo); repo != "" && ok && e.Origin.Origin != "" {
		root = e.Origin.Origin
	}
	_, trace := recall.RecallPicks(root, os.Getenv(memory.SessionIDEnv), query, true, true)
	fmt.Printf("query terms: %s\nembeddings: %s\ncache age: %s; rows: %d\nwould stay silent: %t\n",
		strings.Join(trace.Terms, " "), trace.Embeddings, trace.CacheAge, trace.CacheRows, trace.Silent)
	files := recallExplainFiles(trace.Rows)
	fmt.Println("id        file tier BM25 match   cosine blend boost final outcome")
	for i, row := range trace.Rows {
		if len(row.ID) > 9 {
			fmt.Printf("id @%d: %s\n", i+1, row.ID)
			row.ID = fmt.Sprintf("@%d", i+1)
		}
		recallExplainRow(row, files[row.File])
	}
	return 0
}

func recallExplainFiles(rows []recall.RecallCandidate) map[string]int {
	files := make(map[string]int)
	for _, row := range rows {
		if _, ok := files[row.File]; !ok {
			files[row.File] = len(files) + 1
			fmt.Printf("file %d: %s\n", files[row.File], row.File)
		}
	}
	fmt.Println("tier: c=current, a=archive; match: matched/needed terms")
	return files
}

func recallExplainRow(row recall.RecallCandidate, file int) {
	cosine := "no vector"
	if row.Cosine != nil {
		cosine = recallExplainNumber(*row.Cosine)
	}
	fmt.Printf("%s %d %.1s %s %d/%d %s %s %s %s %s\n",
		row.ID, file, row.Tier, recallExplainNumber(row.BM25), row.Matched, row.Needed,
		cosine, recallExplainNumber(row.Blended), recallExplainNumber(row.AccessBoost),
		recallExplainNumber(row.Final), row.Outcome)
}

func recallExplainNumber(value float64) string {
	s := fmt.Sprintf("%.2g", value)
	if len(s) > 6 {
		s = fmt.Sprintf("%.1g", value)
	}
	s = strings.ReplaceAll(s, "e+", "e")
	s = strings.ReplaceAll(s, "e-0", "e-")
	return strings.ReplaceAll(s, "e0", "e")
}
