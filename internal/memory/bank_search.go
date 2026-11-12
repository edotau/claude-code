package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
	return truncateLine(strings.TrimLeft(stripID(textutil.FirstLine(h.Block)), "#- "))
}

func contentTerms(s string) []string {
	return contentQueryTerms(textutil.Tokenize(s))
}

// contentQueryTerms filters already tokenized query terms without changing their order or multiplicity.
func contentQueryTerms(tokens []string) []string {
	var out []string
	for _, t := range tokens {
		if !stopwords[t] {
			out = append(out, t)
		}
	}
	return out
}

// corpusBlocks loads every searchable block; a vanished file is skipped, any other read error fails the corpus.
func corpusBlocks(root string) ([]SearchHit, error) {
	var out []SearchHit
	for _, f := range corpusFiles(root) {
		data, err := os.ReadFile(f.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		base := filepath.Base(f.Path)
		for _, block := range parseBlocks(f.Mode, base, data) {
			out = append(out, SearchHit{Bank: f.Bank, File: base, Block: block, Dir: ownerDir(f.Bank, f.Path)})
		}
	}
	return out, nil
}

// Entry is one discrete memory item (the ADK MemoryEntry shape): a bank block with id, date and type metadata.
type Entry struct {
	ID         string
	Date       string // YYYY-MM-DD from the title, "" when the block carries none
	Type       string // decision | convention | progress | session | context | topic | knowledge
	Title      string
	Body       string // the whole block, id markers stripped
	Bank, File string
	Archived   bool
	Superseded bool
}

var (
	entryDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	anyIDMark = regexp.MustCompile(`\s*<!-- id:[^>]*-->`) // persisted 8-hex ids and alias ids alike
	// entryTypes maps a typed bank file to its entry type.
	entryTypes = map[string]string{
		"decisionLog.md": "decision", "conventions.md": "convention", "progress.md": "progress",
		"sessionHistory.md": "session", "activeContext.md": "context", "projectContext.md": "context",
	}
)

// isSupersededTitle reports a struck-out or SUPERSEDED title line.
func isSupersededTitle(first string) bool {
	return strings.Contains(first, "~~") || strings.Contains(first, "SUPERSEDED")
}

// entryFromHit lifts a search hit to an Entry; a block without a persisted id gets its contentRef.
func entryFromHit(h SearchHit) Entry {
	id := blockRef(h.Block, h.ID)
	typ := entryTypes[logicalName(h.File)]
	switch {
	case typ != "":
	case h.Bank == "knowledge":
		typ = "knowledge"
	default:
		typ = "topic"
	}
	title := strings.TrimLeft(anyIDMark.ReplaceAllString(textutil.FirstLine(h.Block), ""), "#- ")
	return Entry{
		ID: id, Date: entryDate.FindString(title), Type: typ, Title: truncateLine(title),
		Body: strings.TrimSpace(anyIDMark.ReplaceAllString(h.Block, "")), Bank: h.Bank, File: h.File,
		Archived: h.Bank == "archive", Superseded: isSupersededTitle(title),
	}
}

// pointer is the one-line reference `memory search <id>` expands: id · date · title.
func (e Entry) pointer() string {
	date := e.Date
	if date == "" {
		date = "undated"
	}
	return e.ID + " · " + date + " · " + e.Title
}

// typeRank orders entry types for recall: decisions and conventions first, session narrative last.
func (e Entry) typeRank() int {
	switch e.Type {
	case "decision", "convention":
		return 0
	case "session":
		return 2
	}
	return 1
}
