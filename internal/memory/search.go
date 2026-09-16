package memory

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/textsim"
)

// BM25 defaults; a block's first line is its title and weighs 3x.
const (
	searchK1          = 1.5
	searchB           = 0.75
	searchTitleWeight = 3
)

// stopwords are function words dropped from queries and blocks so paraphrases still rank.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "so": true, "of": true, "and": true, "or": true,
	"for": true, "in": true, "on": true, "at": true, "is": true, "are": true, "be": true, "it": true,
	"its": true, "this": true, "that": true, "with": true, "as": true, "by": true, "always": true,
	"just": true, "then": true, "than": true, "we": true, "you": true, "your": true,
}

// SearchHit is one matching bank block.
type SearchHit struct {
	Score float64 `json:"score"`
	Bank  string  `json:"bank"` // local | global | archive
	File  string  `json:"file"`
	Block string  `json:"block"`
}

// Title is the hit's first line, marker-stripped and truncated.
func (h SearchHit) Title() string {
	first, _, _ := strings.Cut(h.Block, "\n")
	return truncateLine(strings.TrimLeft(first, "#- "))
}

func contentTerms(s string) []string {
	var out []string
	for _, t := range textsim.Tokenize(s) {
		if !stopwords[t] {
			out = append(out, t)
		}
	}
	return out
}

// SearchBanks ranks every block of root's local bank, the global bank and the local archive; top k (0 = all).
func SearchBanks(root, query string, k int) []SearchHit {
	qTerms := contentTerms(query)
	if len(qTerms) == 0 {
		return nil
	}
	type doc struct {
		hit SearchHit
		tf  map[string]float64
		len float64
	}
	var docs []doc
	df := map[string]int{}
	for _, src := range searchSources(root) {
		for _, path := range src.files {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			_, blocks := entryBlocks(filepath.Base(path), string(data))
			for _, block := range blocks {
				tf := map[string]float64{}
				var dl float64
				first, rest, _ := strings.Cut(block, "\n")
				for _, t := range contentTerms(first) {
					tf[t] += searchTitleWeight
					dl += searchTitleWeight
				}
				for _, t := range contentTerms(rest) {
					tf[t]++
					dl++
				}
				if dl == 0 {
					continue
				}
				docs = append(docs, doc{SearchHit{Bank: src.bank, File: filepath.Base(path), Block: block}, tf, dl})
				for t := range tf {
					df[t]++
				}
			}
		}
	}
	if len(docs) == 0 {
		return nil
	}
	var avgdl float64
	for _, d := range docs {
		avgdl += d.len
	}
	n := float64(len(docs))
	avgdl /= n
	var hits []SearchHit
	for _, d := range docs {
		var score float64
		for _, t := range qTerms {
			tf := d.tf[t]
			if tf == 0 {
				continue
			}
			idf := math.Log((n-float64(df[t])+0.5)/(float64(df[t])+0.5) + 1)
			score += idf * tf * (searchK1 + 1) / (tf + searchK1*(1-searchB+searchB*d.len/avgdl))
		}
		if score > 0 {
			d.hit.Score = score
			hits = append(hits, d.hit)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}

type searchSource struct {
	bank  string
	files []string
}

// searchSources: local (unless it IS global), global, local archive; index.md is derived and never a source.
func searchSources(root string) []searchSource {
	bankPaths := func(dir string) []string {
		var fs []string
		for _, name := range bankFiles {
			fs = append(fs, filepath.Join(dir, name))
		}
		return fs
	}
	var out []searchSource
	if !IsGlobal(root) {
		out = append(out, searchSource{"local", bankPaths(Dir(root))})
	}
	out = append(out, searchSource{"global", bankPaths(GlobalDir())})
	if archived, _ := filepath.Glob(filepath.Join(Dir(root), "archive", "*.md")); len(archived) > 0 {
		out = append(out, searchSource{"archive", archived})
	}
	return out
}
