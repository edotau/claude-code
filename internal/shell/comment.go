package shell

import (
	"bytes"
	"strings"
)

// Comments answers, for every byte of one command after a single pass over it, whether a `#` there starts a comment.
type Comments struct {
	text    string
	mask    []bool // inside a closed, unquoted `${…}` or `$((…))`
	bare    []bool // a `)` closing a bare `(` subshell
	hasCase bool
}

// ScanComments indexes cmd for At queries in O(1) each.
func ScanComments(cmd string) Comments {
	s := newCommentScan(cmd, false)
	s.run()
	return Comments{text: cmd, mask: s.mask, bare: s.bare, hasCase: strings.Contains(cmd, "case")}
}

// At: the `#` at cmd[i] starts a word once `\<newline>` is removed; `$(x)#y` stays one word.
func (c Comments) At(i int) bool { return !c.mask[i] && wordStart(c.text, c.bare, c.hasCase, i) }

// DropComments cuts each unquoted `#` that starts a word through the end of its line, judged on the text left so far.
func DropComments(cmd string) string {
	s := newCommentScan(cmd, true)
	if from, slow := s.run(); slow {
		return dropCommentsFrom(string(s.buf)+cmd[from:], len(s.buf))
	}
	return string(s.buf)
}

// dropCommentsFrom is DropComments re-indexing after every cut: the exact path once a later cut can close a `${`.
func dropCommentsFrom(cmd string, from int) string {
	for {
		idx, at, i := ScanComments(cmd), QuoteAt(cmd), -1
		for k := from; k < len(cmd) && i < 0; k++ {
			if cmd[k] == '#' && at[k] == 0 && idx.At(k) {
				i = k
			}
		}
		if i < 0 {
			return cmd
		}
		end := strings.IndexByte(cmd[i:], '\n')
		if end < 0 {
			return cmd[:i]
		}
		cmd, from = cmd[:i]+cmd[i+end:], i+1
	}
}

// wordStart is At's look back from text[i]: a blank, separator or subshell `)` ends the word before it.
func wordStart[T string | []byte](text T, bare []bool, hasCase bool, i int) bool {
	for i > 0 {
		c := text[i-1]
		if strings.IndexByte(" \t\n;&|()", c) < 0 {
			return false
		}
		if !escaped(text, i-1) {
			return c != ')' || !hasCase && bare[i-1] // a `case` pattern's `)` would mis-pair the parens
		}
		if c != '\n' {
			return false // an escaped blank or separator is a literal byte of the word
		}
		i -= 2 // a line continuation is deleted, joining the `#` to what came before it
	}
	return true
}

// commentScan walks src once, keeping (in buf) the text left after the comments it cuts in cut mode.
type commentScan struct {
	src        string
	cut        bool
	buf        []byte // cut mode: the kept text
	prevAt     byte   // QuoteAt mark of the last kept byte
	mask, bare []bool
	quote      byte
	rangeEnd   int    // buf index of the last byte of the open `${…}`/`$((…))`, or -1
	open       []bool // the unclosed `(`s: true for a bare subshell
	broken     bool   // an unmatched `)` was seen: no later `)` closes a subshell
	fragile    bool   // an unclosed `${`/`$((` that a later cut could close
	caseIn     bool   // buf holds "case"
	lastCase   int
}

func newCommentScan(src string, cut bool) *commentScan {
	return &commentScan{src: src, cut: cut, rangeEnd: -1, lastCase: strings.LastIndex(src, "case"),
		mask: make([]bool, 0, len(src)), bare: make([]bool, 0, len(src))}
}

// run scans src; slow reports a cut at from after a fragile opener, where only re-indexing stays exact.
func (s *commentScan) run() (from int, slow bool) {
	for x := 0; x < len(s.src); x++ {
		j, mark := s.chunk(x)
		if mark == 0 && s.cut && s.src[x] == '#' && s.commentHere(x) {
			if s.fragile {
				return x, true
			}
			nl := strings.IndexByte(s.src[x:], '\n')
			if nl < 0 {
				return len(s.src), false
			}
			x += nl - 1 // the newline is kept
			continue
		}
		for y := x; y <= j; y++ {
			s.emit(y, mark)
		}
		x = j
	}
	return len(s.src), false
}

// commentHere is At for the `#` at src[x] on the text cut so far plus the uncut rest.
func (s *commentScan) commentHere(x int) bool {
	k := len(s.buf)
	return k > s.rangeEnd && wordStart(s.buf, s.bare, s.caseIn || s.lastCase >= x, k)
}

// chunk is QuoteAt's step at src[x]: the last index it covers and their mark.
func (s *commentScan) chunk(x int) (int, byte) {
	if q := s.quote; q != 0 {
		j, next := quotedStep(s.src, x, q)
		s.quote = next
		return j, quoteMark(q)
	}
	if j, q, ok := openQuote(s.src, x); ok {
		s.quote = q
		return j, quoteMark(q)
	}
	return x, 0
}

// quoteMark is QuoteAt's mark for quote state q: `"` for a double quote, `'` for the rest.
func quoteMark(q byte) byte {
	if q == '"' {
		return '"'
	}
	return '\''
}

// emit keeps src[y] with QuoteAt mark: its expansion mask, its paren pairing and the "case" flag.
func (s *commentScan) emit(y int, mark byte) {
	k, c := len(s.mask), s.src[y]
	if mark == 0 && c == '$' && k > s.rangeEnd {
		s.opener(y, k)
	}
	inRange, bare := k <= s.rangeEnd, false
	if mark == 0 && !inRange {
		switch c {
		case '(':
			s.open = append(s.open, k == 0 || s.prevAt == 0 && strings.IndexByte(" \t\n;&|(", s.src[y-1]) >= 0)
		case ')':
			bare = s.closeParen()
		}
	}
	s.mask, s.bare, s.prevAt = append(s.mask, inRange), append(s.bare, bare), mark
	if s.cut {
		s.buf = append(s.buf, c)
		s.caseIn = s.caseIn || c == 'e' && bytes.HasSuffix(s.buf, []byte("case"))
	}
}

// opener opens the `${…}` or `$((…))` range at src[y] (buf[k]); an unclosed one marks the scan fragile.
func (s *commentScan) opener(y, k int) {
	end := braceExpansionEnd(s.src, y)
	fragile := end < 0 && braceAt(s.src, y+1)
	if end < 0 && strings.HasPrefix(s.src[y+1:], "((") {
		end = arithEnd(s.src, y)
		fragile = end < 0
	}
	s.fragile = s.fragile || fragile
	if end >= 0 {
		s.rangeEnd = k + end - y
	}
}

// closeParen pops the `(` a `)` closes and reports it bare; an unmatched `)` ends all subshell pairing.
func (s *commentScan) closeParen() bool {
	if s.broken || len(s.open) == 0 {
		s.broken = true
		return false
	}
	bare := s.open[len(s.open)-1]
	s.open = s.open[:len(s.open)-1]
	return bare
}

// exprMask marks each byte of an unquoted, closed `${…}` or `$((…))`: a `#`, `<` or `>` there is word text.
func exprMask(cmd string) []bool {
	s := newCommentScan(cmd, false)
	s.run()
	return s.mask
}

// arithEnd is the last `)` of the `$((…))` at cmd[i], or -1 when its parens close apart: `$((a) b)` is a substitution.
func arithEnd(cmd string, i int) int {
	depth, quote := 0, byte(0)
	for j := i + 1; j < len(cmd); j++ {
		if quote != 0 {
			j, quote = quotedStep(cmd, j, quote)
			continue
		}
		if next, q, ok := openQuote(cmd, j); ok {
			j, quote = next, q
			continue
		}
		switch cmd[j] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 1 {
				if j+1 < len(cmd) && cmd[j+1] == ')' {
					return j + 1
				}
				return -1
			}
		}
	}
	return -1
}
