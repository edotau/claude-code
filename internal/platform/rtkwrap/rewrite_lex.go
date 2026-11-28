// Ported from rtk v1.1.0 src/discover/lexer.rs and shell_wrapper.rs (Apache 2.0, github.com/rtk-ai/rtk).
package rtkwrap

import (
	"slices"
	"strings"
)

type tokKind uint8

const (
	tkArg tokKind = iota
	tkOp
	tkPipe
	tkPipeErr
	tkRedirect
	tkShellism
)

// token is one lexer token; its source span is [off, off+len(val)) because the lexer keeps raw text.
type token struct {
	kind tokKind
	val  string
	off  int
}

func (t token) isPipe() bool { return t.kind == tkPipe || t.kind == tkPipeErr }

type nlMode uint8

const (
	nlNone nlMode = iota
	nlBash
	nlConservative
)

// lexer walks bytes, not runes: every byte rtk's char lexer acts on is ASCII, so the spans come out the same.
type lexer struct {
	in      string
	i       int
	mode    nlMode
	toks    []token
	cur     []byte
	start   int
	quote   byte
	escaped bool
}

func tokenize(in string) []token { return tokenizeMode(in, nlNone) }

func tokenizeMode(in string, mode nlMode) []token {
	lx := &lexer{in: in, mode: mode}
	for lx.i < len(in) {
		lx.step()
	}
	if lx.escaped {
		lx.cur = append(lx.cur, '\\')
	}
	lx.flush()
	return lx.toks
}

func advanceQuote(quote, c byte) byte {
	switch {
	case quote == 0 && (c == '\'' || c == '"'):
		return c
	case quote != 0 && c == quote:
		return 0
	}
	return quote
}

func (lx *lexer) step() {
	c := lx.in[lx.i]
	switch {
	case lx.escaped:
		lx.cur = append(lx.cur, '\\', c)
		lx.i++
		lx.escaped = false
	case c == '\\' && lx.quote != '\'':
		lx.escaped = true
		if len(lx.cur) == 0 {
			lx.start = lx.i
		}
		lx.i++
	case lx.quote != 0 || c == '\'' || c == '"':
		if lx.quote == 0 && len(lx.cur) == 0 {
			lx.start = lx.i
		}
		lx.quote = advanceQuote(lx.quote, c)
		lx.cur = append(lx.cur, c)
		lx.i++
	default:
		lx.bare(c)
	}
}

// lexActions are the unquoted bytes with their own token rule; any other byte joins the current word.
var lexActions = map[byte]func(*lexer){
	'$': (*lexer).dollar, '|': (*lexer).pipe, ';': (*lexer).semi, '&': (*lexer).amp,
	'>': (*lexer).greater, '<': (*lexer).less,
	'*': (*lexer).shellism, '?': (*lexer).shellism, '`': (*lexer).shellism, '(': (*lexer).shellism,
	')': (*lexer).shellism, '{': (*lexer).shellism, '}': (*lexer).shellism, '!': (*lexer).shellism,
}

func (lx *lexer) bare(c byte) {
	if act, ok := lexActions[c]; ok {
		act(lx)
		return
	}
	if lx.newlineOp(c) {
		lx.flush()
		lx.emit(tkOp, "\n", lx.i)
		lx.i++
		lx.start = lx.i
		return
	}
	if c == ' ' || c == '\t' || c == '\n' {
		lx.flush()
		lx.i++
		lx.start = lx.i
		return
	}
	if len(lx.cur) == 0 {
		lx.start = lx.i
	}
	lx.cur = append(lx.cur, c)
	lx.i++
}

// newlineOp: \n, and the \r of CRLF, end a line when the mode splits lines; conservative mode splits on a lone \r too.
func (lx *lexer) newlineOp(c byte) bool {
	if lx.mode == nlNone || c != '\n' && c != '\r' {
		return false
	}
	return c == '\n' || lx.mode == nlConservative || crlfAt(lx.in, lx.i)
}

func crlfAt(s string, i int) bool { return i+1 < len(s) && s[i] == '\r' && s[i+1] == '\n' }

func (lx *lexer) peek() byte {
	if lx.i < len(lx.in) {
		return lx.in[lx.i]
	}
	return 0
}

func (lx *lexer) emit(kind tokKind, val string, off int) {
	lx.toks = append(lx.toks, token{kind: kind, val: val, off: off})
}

func (lx *lexer) flush() {
	if len(lx.cur) > 0 {
		lx.emit(tkArg, string(lx.cur), lx.start)
		lx.cur = lx.cur[:0]
	}
}

func (lx *lexer) dollar() {
	lx.flush()
	start := lx.i
	lx.i++
	if c := lx.peek(); isAlpha(c) || c == '_' {
		for lx.i < len(lx.in) && (isAlpha(lx.in[lx.i]) || isDigit(lx.in[lx.i]) || lx.in[lx.i] == '_') {
			lx.i++
		}
		lx.emit(tkArg, lx.in[start:lx.i], start)
	} else {
		lx.emit(tkShellism, "$", start)
	}
	lx.start = lx.i
}

func isAlpha(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func (lx *lexer) shellism() {
	lx.flush()
	lx.emit(tkShellism, lx.in[lx.i:lx.i+1], lx.i)
	lx.i++
	lx.start = lx.i
}

// operator consumes the first of nexts that follows the current byte and emits the matched text as one token.
func (lx *lexer) operator(nexts string, kinds []tokKind) {
	lx.flush()
	start := lx.i
	lx.i++
	kind := kinds[len(kinds)-1]
	if j := strings.IndexByte(nexts, lx.peek()); j >= 0 && lx.peek() != 0 {
		lx.i++
		kind = kinds[j]
	}
	lx.emit(kind, lx.in[start:lx.i], start)
	lx.start = lx.i
}

func (lx *lexer) pipe() { lx.operator("|&", []tokKind{tkOp, tkPipeErr, tkPipe}) }

func (lx *lexer) semi() {
	lx.flush()
	start := lx.i
	lx.i++
	lx.skipByte(';')
	lx.skipByte('&')
	lx.emit(tkOp, lx.in[start:lx.i], start)
	lx.start = lx.i
}

func (lx *lexer) skipByte(c byte) bool {
	if lx.peek() == c && c != 0 {
		lx.i++
		return true
	}
	return false
}

func (lx *lexer) amp() {
	if lx.i+1 < len(lx.in) && lx.in[lx.i+1] == '>' {
		lx.flush()
		start := lx.i
		lx.i += 2
		lx.skipByte('>')
		lx.emit(tkRedirect, lx.in[start:lx.i], start)
		lx.start = lx.i
		return
	}
	lx.operator("&", []tokKind{tkOp, tkShellism})
}

// fdPrefix takes an all-digit current word as the redirect's descriptor, else flushes it; it returns the start.
func (lx *lexer) fdPrefix() int {
	if len(lx.cur) > 0 && strings.Trim(string(lx.cur), "0123456789") == "" {
		lx.cur = lx.cur[:0]
		return lx.start
	}
	lx.flush()
	return lx.i
}

func (lx *lexer) skipDup() {
	for lx.i < len(lx.in) && (isDigit(lx.in[lx.i]) || lx.in[lx.i] == '-') {
		lx.i++
	}
}

func (lx *lexer) greater() {
	start := lx.fdPrefix()
	lx.i++
	lx.skipByte('>')
	if lx.skipByte('&') {
		lx.skipDup()
	}
	lx.emit(tkRedirect, lx.in[start:lx.i], start)
	lx.start = lx.i
}

func (lx *lexer) less() {
	start := lx.fdPrefix()
	lx.i++
	if !lx.skipByte('<') && lx.skipByte('&') {
		lx.skipDup()
	}
	lx.emit(tkRedirect, lx.in[start:lx.i], start)
	lx.start = lx.i
}

// quoteScan yields each byte outside a backslash escape with the quote state before it; it reports quotes balanced.
func quoteScan(s string, yield func(i int, b byte, sq, dq bool) bool) bool {
	var quote byte
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == '\\' && quote != '\'' {
			i++
			continue
		}
		if !yield(i, b, quote == '\'', quote == '"') {
			return quote == 0
		}
		quote = advanceQuote(quote, b)
	}
	return quote == 0
}

// ansiCDefeatsLexer: an odd backslash run before ' inside $'…' keeps bash's string open where the lexer closes it.
func ansiCDefeatsLexer(cmd string) bool {
	ansi, run, prev, found := false, 0, byte(0), false
	quoteScan(cmd, func(_ int, b byte, sq, dq bool) bool {
		switch {
		case b == '\'' && !dq && !sq:
			ansi, run = prev == '$', 0
		case b == '\'' && !dq:
			found = ansi && run%2 == 1
		case sq && b == '\\':
			run++
		case sq:
			run = 0
		}
		prev = b
		return !found
	})
	return found
}

// containsSubstitution is a backtick or $( outside single quotes, or <( >( outside any quote.
func containsSubstitution(cmd string) bool {
	sq, dq := false, false
	for i := 0; i < len(cmd); i++ {
		b := cmd[i]
		if b == '\\' && !sq {
			i++
			continue
		}
		if !sq && substAt(cmd, i, dq) {
			return true
		}
		sq = sq != (b == '\'' && !dq)
		dq = dq != (b == '"' && !sq)
	}
	return false
}

// substAt reports a substitution opening at cmd[i] outside single quotes; dq is whether double quotes are open.
func substAt(cmd string, i int, dq bool) bool {
	b, next := cmd[i], byte(0)
	if i+1 < len(cmd) {
		next = cmd[i+1]
	}
	return b == '`' || next == '(' && (b == '$' || !dq && (b == '<' || b == '>'))
}

func redirectTakesOperand(v string) bool {
	pos := strings.Index(v, ">&")
	if pos < 0 {
		pos = strings.Index(v, "<&")
	}
	if pos < 0 {
		return true
	}
	tail := v[pos+2:]
	return tail == "" || strings.Trim(tail, "0123456789-") != ""
}

func redirectHasFileTarget(toks []token, i int) bool {
	if !redirectTakesOperand(toks[i].val) {
		return false
	}
	return i+1 >= len(toks) || toks[i+1].kind != tkArg || toks[i+1].val != "/dev/null"
}

type dialect uint8

const (
	posix dialect = iota
	zsh
	fish
)

// unattestable is rtk's contains_unattestable_construct_in: what its permission gate cannot decompose.
func unattestable(cmd string, d dialect) bool {
	if containsSubstitution(cmd) || ansiCDefeatsLexer(cmd) {
		return true
	}
	if d != posix && disownPair(tokenizeMode(cmd, nlConservative)) {
		return true
	}
	if d == fish && fishOnly(tokenizeMode(cmd, nlConservative)) {
		return true
	}
	toks := tokenize(cmd)
	for i, t := range toks {
		if t.kind == tkRedirect && redirectHasFileTarget(toks, i) {
			return true
		}
	}
	return false
}

var fishKeywords = []string{"and", "or", "not", "begin", "end", "if", "else", "switch", "case", "for", "while", "function"}

func fishOnly(toks []token) bool {
	if disownPair(toks) {
		return true
	}
	cmdPos := true
	for _, t := range toks {
		if t.kind == tkShellism && (t.val == "(" || t.val == ")") {
			return true
		}
		if t.kind == tkArg && cmdPos && slices.Contains(fishKeywords, t.val) {
			return true
		}
		cmdPos = fishCmdPos(t, cmdPos)
	}
	return false
}

// fishCmdPos is whether the token after t stands at command position.
func fishCmdPos(t token, cmdPos bool) bool {
	switch {
	case t.kind == tkOp || t.isPipe() || t.kind == tkShellism && t.val == "&":
		return true
	case t.kind == tkArg:
		return false
	}
	return cmdPos
}

func disownPair(toks []token) bool {
	ampEnd := -1
	for _, t := range toks {
		pairs := t.isPipe() || t.kind == tkShellism && t.val == "!" || t.kind == tkArg && strings.HasPrefix(t.val, "!")
		if ampEnd == t.off && pairs {
			return true
		}
		ampEnd = -1
		if t.kind == tkShellism && t.val == "&" {
			ampEnd = t.off + len(t.val)
		}
	}
	return false
}

// word is a run of adjacent tokens, one shell word, with its start offset.
type word struct {
	raw string
	off int
}

func coalesceWords(cmd string, toks []token) []word {
	var out []word
	start, end := -1, 0
	for _, t := range toks {
		if start >= 0 && t.off != end {
			out = append(out, word{cmd[start:end], start})
			start = -1
		}
		if start < 0 {
			start = t.off
		}
		end = t.off + len(t.val)
	}
	if start >= 0 {
		out = append(out, word{cmd[start:end], start})
	}
	return out
}

// shellSplit is argv-ready words: quotes stripped, escapes resolved as bash would.
func shellSplit(in string) []string {
	words := coalesceWords(in, tokenize(in))
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = resolveWord(w.raw)
	}
	return out
}

func resolveWord(raw string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '\\' && quote != '\'':
			i += resolveEscape(&b, raw, i, quote)
		case c == '\'' || c == '"':
			if q := advanceQuote(quote, c); q != quote {
				quote = q
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// resolveEscape writes what the backslash at raw[i] yields and returns how many following bytes it consumed.
func resolveEscape(b *strings.Builder, raw string, i int, quote byte) int {
	if i+1 >= len(raw) {
		if quote == '"' {
			b.WriteByte('\\')
		}
		return 0
	}
	if quote == '"' && strings.IndexByte("$`\"\\\n", raw[i+1]) < 0 {
		b.WriteByte('\\')
		return 0
	}
	b.WriteByte(raw[i+1])
	return 1
}

func hasHeredoc(cmd string) bool {
	for _, t := range tokenize(cmd) {
		if t.kind == tkRedirect && strings.HasPrefix(t.val, "<<") {
			return true
		}
	}
	return false
}

// shellWrapper is a quoted `sh|bash|zsh|fish -c '…'` script span inside its command.
type shellWrapper struct {
	start, end int
	dialect    dialect
}

var wrapperShells = map[string]dialect{"sh": posix, "bash": posix, "zsh": zsh, "fish": fish}

func parseShellWrapper(cmd string) (shellWrapper, bool) {
	pos, d, ok := wrapperShell(cmd)
	if !ok {
		return shellWrapper{}, false
	}
	pos = skipBlank(cmd, pos+2)
	if pos >= len(cmd) || cmd[pos] != '\'' && cmd[pos] != '"' {
		return shellWrapper{}, false
	}
	end, ok := scriptEnd(cmd, pos+1, cmd[pos])
	if !ok || end == pos+1 || end+1 < len(cmd) && !isBlank(cmd[end+1]) {
		return shellWrapper{}, false
	}
	return shellWrapper{start: pos + 1, end: end, dialect: d}, true
}

// wrapperShell finds a supported shell word followed by a blank-separated -c; pos is where -c starts.
func wrapperShell(cmd string) (pos int, d dialect, ok bool) {
	sp := strings.IndexAny(cmd, " \t")
	if strings.IndexByte(cmd, 0) >= 0 || sp < 0 {
		return 0, posix, false
	}
	shellName := cmd[:sp]
	d, ok = wrapperShells[shellName[strings.LastIndexAny(shellName, `/\`)+1:]]
	pos = skipBlank(cmd, sp)
	ok = ok && strings.HasPrefix(cmd[pos:], "-c") && pos+2 < len(cmd) && isBlank(cmd[pos+2])
	return pos, d, ok
}

func isBlank(b byte) bool { return b == ' ' || b == '\t' }

func skipBlank(s string, i int) int {
	for i < len(s) && isBlank(s[i]) {
		i++
	}
	return i
}

func scriptEnd(cmd string, start int, quote byte) (int, bool) {
	for i := start; i < len(cmd); i++ {
		b := cmd[i]
		switch {
		case b == quote:
			return i, true
		case b == 0 || b == '\n' || b == '\r':
			return 0, false
		case quote == '"' && (b == '$' || b == '`' || b == '\\'):
			return 0, false
		}
	}
	return 0, false
}
