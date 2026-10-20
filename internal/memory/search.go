package memory

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
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
	ID      string  `json:"id,omitempty"` // persisted block ID (`memory show <id>`)
}

// Title is the hit's first line, marker-stripped and truncated.
func (h SearchHit) Title() string {
	first, _, _ := strings.Cut(h.Block, "\n")
	return truncateLine(strings.TrimLeft(stripID(first), "#- "))
}

func contentTerms(s string) []string {
	return ContentQueryTerms(textsim.Tokenize(s))
}

// ContentQueryTerms filters already tokenized query terms without changing their order or multiplicity.
func ContentQueryTerms(tokens []string) []string {
	var out []string
	for _, t := range tokens {
		if !stopwords[t] {
			out = append(out, t)
		}
	}
	return out
}

// SearchBanks ranks every block of root's local, global and archive banks plus native topics; top k (0 = all).
func SearchBanks(root, query string, k int) []SearchHit {
	return SearchBanksTerms(root, query, contentTerms(query), k)
}

// SearchBanksTerms ranks banks using the caller's pretokenized content terms.
func SearchBanksTerms(root, query string, terms []string, k int) []SearchHit {
	files := append(corpusFiles(root), routedFiles(root, query)...)
	return rankFiles(terms, k, files, loadSegments(files, true))
}

// SearchAll ranks the global tier plus every repo store as one corpus (`memory search --all`).
func SearchAll(query string, k int) []SearchHit {
	files := corpusFiles(paths.ConfigDir())
	if entries, err := RepoDirs(); err == nil {
		for _, e := range entries {
			if e.Key != HarnessKey {
				files = append(files, storeFiles(e)...)
			}
		}
	}
	return rankFiles(contentTerms(query), k, files, loadSegments(files, false))
}

// corpusBlocks loads every searchable block (Score 0): bank entries split by file grammar, a native topic whole.
func corpusBlocks(root string) []SearchHit {
	var out []SearchHit
	for _, f := range corpusFiles(root) {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		base := filepath.Base(f.path)
		for _, block := range parseBlocks(f.mode, base, data) {
			out = append(out, SearchHit{Bank: f.bank, File: base, Block: block})
		}
	}
	return out
}
