package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
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
	Bank    string  `json:"bank"`    // local | home | archive | native
	File    string  `json:"file"`
	Block   string  `json:"block"`
	ID      string  `json:"id,omitempty"` // persisted block ID (`memory show <id>`)
	Dir     string  `json:"-"`
}

// Title is the hit's first line, marker-stripped and truncated.
func (h SearchHit) Title() string {
	first, _, _ := strings.Cut(h.Block, "\n")
	return truncateLine(strings.TrimLeft(StripID(first), "#- "))
}

// Plain is the whole block on one line, id markers and heading/bullet marks stripped, for compact previews.
func (h SearchHit) Plain() string {
	var parts []string
	for l := range strings.SplitSeq(h.Block, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(StripID(l), "#- ")); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

func ContentTerms(s string) []string {
	return ContentQueryTerms(textutil.Tokenize(s))
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

// CorpusBlocks loads every searchable block (Score 0): bank entries split by file grammar, a native topic whole.
// A file that vanished since listing is skipped; any other read error fails the whole corpus.
func CorpusBlocks(root string) ([]SearchHit, error) {
	var out []SearchHit
	for _, f := range CorpusFiles(root) {
		data, err := os.ReadFile(f.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		base := filepath.Base(f.Path)
		for _, block := range ParseBlocks(f.Mode, base, data) {
			out = append(out, SearchHit{Bank: f.Bank, File: base, Block: block, Dir: ownerDir(f.Bank, f.Path)})
		}
	}
	return out, nil
}
