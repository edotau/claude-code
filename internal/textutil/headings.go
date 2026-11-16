package textutil

import (
	"bytes"
	"slices"
	"strings"
)

// Heading is one Markdown heading: level, raw text (lines joined by "\n"), first-line text and first-line byte span.
type Heading struct {
	Level      int
	Text       string
	First      string
	Start, End int
}

// htmlBlockNames are the CommonMark type-6 HTML block tag names (sorted for binary search).
var htmlBlockNames = []string{
	"address", "article", "aside", "base", "basefont", "blockquote", "body", "caption", "center", "col", "colgroup",
	"dd", "details", "dialog", "dir", "div", "dl", "dt", "fieldset", "figcaption", "figure", "footer", "form",
	"frame", "frameset", "h1", "h2", "h3", "h4", "h5", "h6", "head", "header", "hr", "html", "iframe", "legend",
	"li", "link", "main", "menu", "menuitem", "meta", "nav", "noframes", "ol", "optgroup", "option", "p", "param", "search",
	"section", "summary", "table", "tbody", "td", "tfoot", "th", "thead", "title", "tr", "track", "ul",
}

// mdContainer is an open blockquote (width 0) or list item (width = content column) on the container stack.
type mdContainer struct {
	quote bool
	width int
	bare  bool // a list item opened with an empty line, which one blank line then closes
	dead  bool
}

type headingScan struct {
	out       []Heading
	stack     []mdContainer
	fenceCh   byte              // open fence's character, 0 when none
	fenceLen  int               // open fence's length
	htmlClose func(string) bool // closes an open type 1-5 HTML block, given a lowercased line
	htmlBlank bool              // inside a type-6/7 HTML block, which a blank line ends
	leafDepth int               // len(stack) when the open fence or HTML block started
	para      []mdLine
	inPara    bool
}

type mdLine struct {
	start, end int
	text       string // the line past its matched container prefixes
}

// Headings returns src's headings (front matter stripped) as CommonMark finds them, entering quotes and list items.
func Headings(src []byte) []Heading {
	var s headingScan
	for pos := 0; pos < len(src); {
		next := len(src)
		if i := bytes.IndexByte(src[pos:], '\n'); i >= 0 {
			next = pos + i + 1
		}
		s.feed(pos, next, strings.TrimSuffix(string(src[pos:next]), "\n"))
		pos = next
	}
	return s.out
}

func (s *headingScan) feed(start, end int, text string) {
	if n := len(s.stack); n > 0 && s.stack[n-1].bare {
		s.stack[n-1].dead = strings.TrimSpace(text) == ""
		s.stack[n-1].bare = false
	}
	rest, matched := s.match(text)
	l := mdLine{start: start, end: end, text: rest}
	if matched < len(s.stack) {
		if s.fenceCh == 0 && s.htmlClose == nil && !s.htmlBlank && s.inPara && lazyText(rest) {
			s.para = append(s.para, l)
			return
		}
		s.pop(matched)
	}
	switch {
	case s.fenceCh != 0:
		if closesFence(rest, s.fenceCh, s.fenceLen) {
			s.fenceCh = 0
		}
		return
	case s.htmlClose != nil:
		if s.htmlClose(strings.ToLower(rest)) {
			s.htmlClose = nil
		}
		return
	case s.htmlBlank:
		if strings.TrimSpace(rest) != "" {
			return
		}
		s.htmlBlank = false
	}
	if strings.TrimSpace(rest) == "" {
		s.endPara()
		return
	}
	s.leaf(l, matched == len(s.stack))
}

// match strips the open containers' prefixes from text and counts how many of them the line continues.
func (s *headingScan) match(text string) (string, int) {
	for i, c := range s.stack {
		if c.dead {
			return text, i
		}
		if strings.TrimSpace(text) == "" && !c.quote {
			continue
		}
		body, ind := leadCols(text)
		switch {
		case c.quote && ind < 4 && strings.HasPrefix(body, ">"):
			text = strings.TrimPrefix(body[1:], " ")
		case !c.quote && ind >= c.width:
			text = stripCols(text, c.width)
		default:
			return text, i
		}
	}
	return text, len(s.stack)
}

func (s *headingScan) pop(n int) {
	s.stack = s.stack[:n]
	if s.fenceCh != 0 && s.leafDepth > n {
		s.fenceCh = 0
	}
	if (s.htmlClose != nil || s.htmlBlank) && s.leafDepth > n {
		s.htmlClose, s.htmlBlank = nil, false
	}
	s.endPara()
}

func (s *headingScan) endPara() { s.inPara, s.para = false, s.para[:0] }

// leaf handles one non-blank line past its containers; full reports it continued every open container.
func (s *headingScan) leaf(l mdLine, full bool) {
	body, ind := leadCols(l.text)
	if ind >= 4 {
		if s.inPara {
			s.para = append(s.para, l)
		}
		return
	}
	if level := setextLevel(body); level != 0 && s.inPara && full {
		s.setext(level)
		return
	}
	if isThematicBreak(body) {
		s.endPara()
		return
	}
	if rest, c, ok := containerOpen(body, ind, s.inPara); ok {
		s.endPara()
		empty := strings.TrimSpace(rest) == ""
		c.bare = empty && !c.quote
		s.stack = append(s.stack, c)
		if !empty {
			l.text = rest
			s.leaf(l, true)
		}
		return
	}
	s.block(l, body)
}

// block handles a line whose first block is a leaf: fence, ATX heading, HTML block or paragraph text.
func (s *headingScan) block(l mdLine, body string) {
	if ch, n := fenceOpen(body); ch != 0 {
		s.fenceCh, s.fenceLen, s.leafDepth = ch, n, len(s.stack)
		s.endPara()
		return
	}
	if level, text, ok := atxHeading(body); ok {
		s.out = append(s.out, Heading{Level: level, Text: text, First: text, Start: l.start, End: l.end})
		s.endPara()
		return
	}
	if closes, blank := htmlBlockStart(body, s.inPara); closes != nil || blank {
		s.endPara()
		s.leafDepth = len(s.stack)
		s.htmlBlank = blank
		if closes != nil && !closes(strings.ToLower(body)) {
			s.htmlClose = closes
		}
		return
	}
	s.inPara = true
	s.para = append(s.para, l)
}

func (s *headingScan) setext(level int) {
	texts := make([]string, len(s.para))
	for i, p := range s.para {
		texts[i] = strings.TrimLeft(p.text, " \t")
	}
	last := len(texts) - 1
	texts[last] = strings.TrimRight(texts[last], " \t\r\f\v")
	first := texts[0]
	if last == 0 {
		first = strings.TrimRight(first, " \t\r\f\v")
	} else {
		first += "\n" // a multi-line heading's first segment keeps its line break, as the AST's does
	}
	s.out = append(s.out, Heading{
		Level: level, Text: strings.Join(texts, "\n"), First: first, Start: s.para[0].start, End: s.para[0].end,
	})
	s.endPara()
}

// leadCols returns text past its leading spaces and tabs, and their width in columns (tab stops of 4).
func leadCols(text string) (string, int) { return leadColsFrom(text, 0) }

// leadColsFrom is leadCols for text that starts at column col, which tab stops depend on.
func leadColsFrom(text string, col int) (string, int) {
	cols := col
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case ' ':
			cols++
		case '\t':
			cols += 4 - cols%4
		default:
			return text[i:], cols - col
		}
	}
	return "", cols - col
}

// stripCols drops leading whitespace worth n columns, leaving the rest of a tab that straddles n as spaces.
func stripCols(text string, n int) string {
	for cols := 0; len(text) > 0 && cols < n; text = text[1:] {
		switch text[0] {
		case ' ':
			cols++
		case '\t':
			w := 4 - cols%4
			if cols+w > n {
				return strings.Repeat(" ", cols+w-n) + text[1:]
			}
			cols += w
		default:
			return text
		}
	}
	return text
}

// containerOpen reports a blockquote or list-item opener on body (ind columns in) and the text after its marker.
func containerOpen(body string, ind int, inPara bool) (string, mdContainer, bool) {
	if body[0] == '>' {
		return strings.TrimPrefix(body[1:], " "), mdContainer{quote: true}, true
	}
	w := 1
	switch {
	case body[0] == '-' || body[0] == '*' || body[0] == '+':
	case body[0] >= '0' && body[0] <= '9':
		for w < len(body) && w < 10 && body[w] >= '0' && body[w] <= '9' {
			w++
		}
		if w >= len(body) || w > 9 || (body[w] != '.' && body[w] != ')') || (inPara && body[:w] != "1") {
			return "", mdContainer{}, false
		}
		w++
	default:
		return "", mdContainer{}, false
	}
	after := body[w:]
	if after != "" && after[0] != ' ' && after[0] != '\t' {
		return "", mdContainer{}, false
	}
	content, gap := leadColsFrom(after, ind+w)
	if content == "" && inPara {
		return "", mdContainer{}, false // an empty item cannot interrupt a paragraph
	}
	if content == "" || gap > 4 {
		content, gap = strings.Repeat(" ", max(gap-1, 0))+content, 1
	}
	return content, mdContainer{width: ind + w + gap}, true
}

// lazyText reports whether rest, as an unmatched line, continues the open paragraph instead of ending its containers.
func lazyText(rest string) bool {
	body, ind := leadCols(strings.TrimRight(rest, "\r"))
	if body == "" {
		return false
	}
	if ind >= 4 {
		return true
	}
	if _, _, ok := atxHeading(body); ok || isThematicBreak(body) {
		return false
	}
	if ch, _ := fenceOpen(body); ch != 0 {
		return false
	}
	if _, _, ok := containerOpen(body, ind, false); ok {
		return false
	}
	closes, blank := htmlBlockStart(body, true)
	return closes == nil && !blank
}

func fenceOpen(body string) (byte, int) {
	if body == "" || (body[0] != '`' && body[0] != '~') {
		return 0, 0
	}
	n := len(body) - len(strings.TrimLeft(body, body[:1]))
	if n < 3 || (body[0] == '`' && strings.Contains(body[n:], "`")) {
		return 0, 0
	}
	return body[0], n
}

func closesFence(t string, ch byte, n int) bool {
	body := strings.TrimLeft(t, " ")
	if len(t)-len(body) > 3 || body == "" || body[0] != ch {
		return false
	}
	run := len(body) - len(strings.TrimLeft(body, body[:1]))
	return run >= n && strings.TrimSpace(body[run:]) == ""
}

// atxHeading parses "# text" (1-6 hashes, a space or end after them, optional closing hashes).
func atxHeading(body string) (level int, text string, ok bool) {
	level = len(body) - len(strings.TrimLeft(body, "#"))
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := strings.TrimRight(body[level:], " \t\r\f\v")
	if rest == "" {
		return level, "", true
	}
	if rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false
	}
	rest = strings.TrimLeft(rest, " \t")
	if strings.Trim(rest, "#") == "" {
		return level, "", true
	}
	j := len(rest)
	for j > 0 && rest[j-1] == '#' {
		j--
	}
	if j < len(rest) && j > 0 && (rest[j-1] == ' ' || rest[j-1] == '\t') {
		rest = strings.TrimRight(rest[:j], " \t")
	}
	return level, rest, true
}

// htmlBlockStart reports how an HTML block opening on body ends: a closer (types 1-5) or a blank line (6, 7).
func htmlBlockStart(body string, inPara bool) (closes func(string) bool, blank bool) {
	if len(body) < 2 || body[0] != '<' {
		return nil, false
	}
	low := strings.ToLower(body)
	contains := func(end string) func(string) bool {
		return func(line string) bool { return strings.Contains(line, end) }
	}
	switch {
	case strings.HasPrefix(low, "<!--"):
		return contains("-->"), false
	case strings.HasPrefix(low, "<?"):
		return contains("?>"), false
	case strings.HasPrefix(low, "<![cdata["):
		return contains("]]>"), false
	case len(body) > 2 && body[1] == '!' && body[2] >= 'A' && body[2] <= 'Z':
		return contains(">"), false
	}
	name := strings.TrimLeft(strings.TrimPrefix(low[1:], "/"), " ")
	n := 0
	for n < len(name) && (isAlpha(name[n]) || isDigit(name[n]) || name[n] == '-') {
		n++
	}
	tail := name[n:]
	spaced := n < len(name) && (name[n] == ' ' || name[n] == '>' || strings.HasPrefix(tail, "/>"))
	if low[1] != '/' && isRawOpen(name[:n]) && (n == len(name) || spaced || name[n] == '\t') {
		return rawClose, false
	}
	if _, found := slices.BinarySearch(htmlBlockNames, name[:n]); found && (n == len(name) || spaced) {
		return nil, true
	}
	switch name[:n] {
	case "script", "style", "pre":
		return nil, false
	}
	return nil, !inPara && completeTag(low)
}

func isRawOpen(name string) bool {
	return name == "script" || name == "pre" || name == "style" || name == "textarea"
}

func rawClose(line string) bool {
	return strings.Contains(line, "</script>") || strings.Contains(line, "</pre>") ||
		strings.Contains(line, "</style>") || strings.Contains(line, "</textarea>")
}

// completeTag reports whether s is one complete open or closing tag followed only by whitespace (HTML block type 7).
func completeTag(s string) bool {
	i := 1
	if i < len(s) && s[i] == '/' {
		i++
	}
	if i >= len(s) || !isAlpha(s[i]) {
		return false
	}
	for i < len(s) && (isAlpha(s[i]) || isDigit(s[i]) || s[i] == '-') {
		i++
	}
	if s[1] == '/' {
		i = skipSpace(s, i)
		return i < len(s) && s[i] == '>' && strings.TrimSpace(s[i+1:]) == ""
	}
	for {
		j := skipSpace(s, i)
		switch {
		case j >= len(s):
			return false
		case s[j] == '>':
			return strings.TrimSpace(s[j+1:]) == ""
		case s[j] == '/':
			return j+1 < len(s) && s[j+1] == '>' && strings.TrimSpace(s[j+2:]) == ""
		case j == i || (!isAlpha(s[j]) && s[j] != '_' && s[j] != ':'):
			return false
		}
		var ok bool
		if i, ok = attrEnd(s, j); !ok {
			return false
		}
	}
}

// attrEnd returns the index past the attribute starting at s[j] (name, optional =value).
func attrEnd(s string, j int) (int, bool) {
	k := j + 1
	for k < len(s) && (isAlpha(s[k]) || isDigit(s[k]) || strings.IndexByte("_.:-", s[k]) >= 0) {
		k++
	}
	m := skipSpace(s, k)
	if m >= len(s) || s[m] != '=' {
		return k, true
	}
	m = skipSpace(s, m+1)
	if m >= len(s) {
		return 0, false
	}
	if q := s[m]; q == '"' || q == '\'' {
		e := strings.IndexByte(s[m+1:], q)
		return m + 2 + e, e >= 0
	}
	k = m
	for k < len(s) && strings.IndexByte(" \t\"'=<>`", s[k]) < 0 {
		k++
	}
	return k, k > m
}

func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func setextLevel(body string) int {
	t := strings.TrimRight(body, " \t\r\f\v")
	if t == "" || strings.Trim(t, t[:1]) != "" {
		return 0
	}
	switch t[0] {
	case '=':
		return 1
	case '-':
		return 2
	}
	return 0
}

func isThematicBreak(body string) bool {
	if body[0] != '-' && body[0] != '*' && body[0] != '_' {
		return false
	}
	n := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case body[0]:
			n++
		case ' ', '\t', '\r':
		default:
			return false
		}
	}
	return n >= 3
}
