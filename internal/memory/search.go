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
	// Conversational filler: a prompt is prose, and one of these matching a block is not relevance.
	"i": true, "me": true, "my": true, "our": true, "us": true, "they": true, "them": true, "there": true,
	"here": true, "can": true, "do": true, "does": true, "did": true, "have": true, "has": true, "had": true,
	"how": true, "what": true, "which": true, "who": true, "why": true, "when": true, "where": true, "if": true,
	"but": true, "not": true, "no": true, "was": true, "were": true, "been": true, "will": true, "would": true,
	"could": true, "should": true, "some": true, "any": true, "all": true, "about": true, "from": true,
	"into": true, "over": true, "more": true, "less": true, "only": true, "also": true, "these": true,
	"those": true, "please": true, "want": true, "need": true, "make": true, "sure": true, "let": true,
}

// SearchHit is one matching bank block.
type SearchHit struct {
	Score   float64 `json:"score"`
	Matched int     `json:"matched"` // distinct query terms the block contains
	Bank    string  `json:"bank"`    // local | global | archive | native
	File    string  `json:"file"`
	Block   string  `json:"block"`
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

// QueryTermCount is the number of distinct content terms in q.
func QueryTermCount(q string) int {
	seen := map[string]bool{}
	for _, t := range contentTerms(q) {
		seen[t] = true
	}
	return len(seen)
}

// SearchBanks ranks every block of root's local, global and archive banks plus native topics; top k (0 = all).
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
	for _, hit := range corpusBlocks(root) {
		tf := map[string]float64{}
		var dl float64
		first, rest, _ := strings.Cut(hit.Block, "\n")
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
		docs = append(docs, doc{hit, tf, dl})
		for t := range tf {
			df[t]++
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
		matched := map[string]bool{}
		for _, t := range qTerms {
			tf := d.tf[t]
			if tf == 0 {
				continue
			}
			matched[t] = true
			idf := math.Log((n-float64(df[t])+0.5)/(float64(df[t])+0.5) + 1)
			score += idf * tf * (searchK1 + 1) / (tf + searchK1*(1-searchB+searchB*d.len/avgdl))
		}
		if score > 0 {
			d.hit.Score = score
			d.hit.Matched = len(matched)
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
	bank   string
	files  []string
	native bool // one-fact topic files with frontmatter, not entry-block bank files
}

// corpusBlocks loads every searchable block (Score 0): bank entries split by file grammar, a native topic whole.
func corpusBlocks(root string) []SearchHit {
	var out []SearchHit
	for _, src := range searchSources(root) {
		for _, path := range src.files {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			base := filepath.Base(path)
			if src.native {
				if name, desc, body, ok := topicFrontmatter(string(data)); ok {
					out = append(out, SearchHit{Bank: src.bank, File: base, Block: name + " — " + desc + "\n" + body})
				}
				continue
			}
			_, blocks := entryBlocks(base, string(data))
			for _, b := range blocks {
				out = append(out, SearchHit{Bank: src.bank, File: base, Block: b})
			}
		}
	}
	return out
}

// searchSources: local (unless it IS global), global, local archive, native topics; index.md is derived, never a source.
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
		out = append(out, searchSource{bank: "local", files: bankPaths(Dir(root))})
	}
	out = append(out, searchSource{bank: "global", files: bankPaths(GlobalDir())})
	if archived, _ := filepath.Glob(filepath.Join(Dir(root), "archive", "*.md")); len(archived) > 0 {
		out = append(out, searchSource{bank: "archive", files: archived})
	}
	for _, dir := range NativeDirs(root) {
		if topics := topicPaths(dir); len(topics) > 0 {
			out = append(out, searchSource{bank: "native", files: topics, native: true})
		}
	}
	return out
}
