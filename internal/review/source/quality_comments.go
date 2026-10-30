package source

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

const maxCommentLineChars = 120

var commentDirectives = []string{"//go:", "//nolint", "//lint:", "// +build", "#!", "# -*-", "# noqa", "# type:",
	"# pragma", "// eslint-", "// @ts-"}

var pyTripleQuotes = []string{`"""`, "'''"}

var commentTags = []string{"@param", "@returns", "@return", "@throws", "@example", "@type"}

var commentExemptStarts = []string{"provenance", "audit", "gotcha", "note:", "warning:"}

var commentSectionHeader = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)^(args|arguments|parameters|params|returns?|yields|raises|throws|usage|examples?|attributes):`)
})

// commentBlock is a run of comment lines; header marks one that starts before the file's first code line.
type commentBlock struct {
	start  int
	lines  []string
	header bool
}

// commentSmells flags stacked prose comment blocks and over-long comment lines (quality.md -> Comments).
func commentSmells(content, lang string) []Smell {
	var smells []Smell
	for _, b := range scanCommentBlocks(content, lang) {
		smells = append(smells, blockSmells(b, lang)...)
	}
	return smells
}

// scanCommentBlocks splits the source into line-comment, /* */ and triple-quote blocks.
func scanCommentBlocks(content, lang string) []commentBlock {
	var blocks []commentBlock
	var cur *commentBlock
	marker, delim, sawCode := "//", "", false
	if lang == "python" {
		marker = "#"
	}
	flush := func() {
		if cur != nil {
			blocks = append(blocks, *cur)
			cur = nil
		}
	}
	for i, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if delim != "" {
			cur.lines = append(cur.lines, line)
			if strings.Contains(t, delim) {
				delim = ""
				flush()
			}
			continue
		}
		open := blockOpener(t, lang)
		switch {
		case t == "":
			flush()
		case strings.HasPrefix(t, marker):
			if cur == nil {
				cur = &commentBlock{start: i + 1, header: !sawCode}
			}
			cur.lines = append(cur.lines, line)
		case open != "":
			flush()
			cur = &commentBlock{start: i + 1, header: !sawCode, lines: []string{line}}
			delim = unclosedDelim(t, open)
			if delim == "" {
				flush()
			}
		default:
			flush()
			sawCode = true
		}
	}
	flush()
	return blocks
}

// blockOpener returns the closing delimiter for a line that opens a block comment or docstring, else "".
func blockOpener(t, lang string) string {
	if lang == "python" {
		for _, q := range pyTripleQuotes {
			if strings.HasPrefix(t, q) {
				return q
			}
		}
		return ""
	}
	if strings.HasPrefix(t, "/*") {
		return "*/"
	}
	return ""
}

// unclosedDelim returns the closer when the opening line does not also close the block, else "".
func unclosedDelim(t, closer string) string {
	if strings.Contains(t[len(closer):], closer) {
		return ""
	}
	return closer
}

// blockSmells evaluates one block: exemptions first, then line length, then the prose-line count.
func blockSmells(b commentBlock, lang string) []Smell {
	prose := proseLines(b, lang)
	if b.header || (len(prose) > 0 && commentExempt(prose[0])) {
		return nil
	}
	var out []Smell
	for i, line := range b.lines {
		if n := utf8.RuneCountInString(line); n > maxCommentLineChars {
			out = append(out, Smell{Type: "long_comment_line", Severity: "low",
				Message:  fmt.Sprintf("Comment line is %d chars (max: %d)", n, maxCommentLineChars),
				Location: fmt.Sprintf("line %d", b.start+i)})
		}
	}
	if len(prose) > 1 {
		out = append(out, Smell{Type: "long_comment", Severity: "low",
			Message:  fmt.Sprintf("Comment block has %d lines (max: 1)", len(prose)),
			Location: fmt.Sprintf("line %d", b.start)})
	}
	return out
}

func commentExempt(first string) bool {
	return hasAnyPrefix(strings.ToLower(first), commentExemptStarts)
}

// proseLines returns the block's prose text, dropping blanks, directives, tags, underlines and section bodies.
func proseLines(b commentBlock, lang string) []string {
	var prose []string
	for _, line := range b.lines {
		t := strings.TrimSpace(line)
		if hasAnyPrefix(t, commentDirectives) {
			continue
		}
		text := commentText(t, lang)
		if commentSectionHeader().MatchString(text) {
			break
		}
		if text == "" || strings.Trim(text, "-=") == "" || hasAnyPrefix(text, commentTags) {
			continue
		}
		prose = append(prose, text)
	}
	return prose
}

// commentText strips comment markers and delimiters from a trimmed comment line.
func commentText(t, lang string) string {
	if lang == "python" {
		t = strings.TrimPrefix(t, "#")
		for _, q := range pyTripleQuotes {
			t = strings.ReplaceAll(t, q, "")
		}
		return strings.TrimSpace(t)
	}
	t = strings.TrimPrefix(t, "//")
	t = strings.TrimPrefix(t, "/*")
	t = strings.TrimSuffix(t, "*/")
	t = strings.TrimPrefix(strings.TrimSpace(t), "*")
	return strings.TrimSpace(t)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(s, p) })
}
