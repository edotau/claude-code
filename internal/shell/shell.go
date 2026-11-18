// Package shell parses shell command text for hooks: segments, words, substitutions, wrappers and git invocations.
package shell

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Segments splits on unquoted command separators (`&&`, `||`, `;`, `|`, `&`, newline).
func Segments(cmd string) []string {
	segs, _ := SegmentsSep(cmd)
	return segs
}

// SegmentsSep is Segments plus the separator that ended each segment ("" for the last).
func SegmentsSep(cmd string) (out, seps []string) {
	start := 0
	var quote byte
	for i := 0; i < len(cmd); i++ {
		if quote != 0 {
			i, quote = quotedStep(cmd, i, quote)
			continue
		}
		if next, q, ok := openQuote(cmd, i); ok {
			i, quote = next, q
			continue
		}
		if separatorStarts(cmd, i) {
			sep, end := separatorAt(cmd, i)
			out, seps = append(out, cmd[start:i]), append(seps, sep)
			start, i = end+1, end
		}
	}
	return append(out, cmd[start:]), append(seps, "")
}

// QuoteAt marks each byte: 0 outside quotes, ' inside '…', $'…' or after a backslash, " inside "…".
func QuoteAt(cmd string) []byte {
	at := make([]byte, len(cmd))
	var quote byte
	for i := 0; i < len(cmd); i++ {
		if quote != 0 {
			j, q := quotedStep(cmd, i, quote)
			markQuoted(at, i, j, quote)
			i, quote = j, q
			continue
		}
		if next, q, ok := openQuote(cmd, i); ok {
			markQuoted(at, i, next, q)
			i, quote = next, q
		}
	}
	return at
}

// markQuoted fills at[from..to] with the double-quote mark for "…" and the single-quote mark for everything else.
func markQuoted(at []byte, from, to int, quote byte) {
	kind := byte('\'')
	if quote == '"' {
		kind = '"'
	}
	for k := from; k <= to; k++ {
		at[k] = kind
	}
}

// openQuote reports an opener at s[i] (`$'`, `'`, `"`, backslash): its last byte and the quote state after it.
func openQuote(s string, i int) (next int, quote byte, ok bool) {
	switch c := s[i]; {
	case c == '$' && i+1 < len(s) && s[i+1] == '\'':
		return i + 1, ansiQuote, true
	case c == '\'' || c == '"':
		return i, c, true
	case c == '\\' && i+1 < len(s):
		return i + 1, 0, true
	}
	return i, 0, false
}

// escaped reports an odd run of backslashes right before cmd[i].
func escaped[T string | []byte](cmd T, i int) bool {
	n := 0
	for i-n-1 >= 0 && cmd[i-n-1] == '\\' {
		n++
	}
	return n%2 == 1
}

// separatorStarts reports a command separator at cmd[i]; `2>&1`, `<&3`, `&>file` and `>|file` redirections are not one.
func separatorStarts(cmd string, i int) bool {
	return strings.IndexByte(";|&\n", cmd[i]) >= 0 && (cmd[i] != '&' || !redirectAmp(cmd, i)) &&
		(cmd[i] != '|' || !redirectAfter(cmd, i, ">"))
}

// redirectAmp reports a `&` that follows `>`/`<` or precedes `>`, a redirection rather than a separator.
func redirectAmp(cmd string, i int) bool {
	return redirectAfter(cmd, i, "<>") || i+1 < len(cmd) && cmd[i+1] == '>'
}

// redirectAfter reports that cmd[i] follows an unescaped byte of ops: `\>&` is a literal `>` and a background `&`.
func redirectAfter(cmd string, i int, ops string) bool {
	return i > 0 && strings.IndexByte(ops, cmd[i-1]) >= 0 && !escaped(cmd, i-1)
}

// separatorAt is the separator starting at cmd[i] (one byte, or two when `|`/`&` follows) and its last index.
func separatorAt(cmd string, i int) (string, int) {
	if i+1 < len(cmd) && (cmd[i+1] == '|' || cmd[i+1] == '&') {
		return cmd[i : i+2], i + 1
	}
	return cmd[i : i+1], i
}

// Tokens splits one segment into unquoted words, dropping a leading `(`/`{` so a subshell reads as its segment.
func Tokens(seg string) []string {
	var out []string
	var buf []byte
	var quote byte
	started, raw, wordStart := false, true, 0
	for i := 0; i < len(seg); i++ {
		if lineContinuation(seg, i, quote) {
			if !started {
				wordStart = i + 2
			} else if raw {
				buf, raw = append(buf[:0], seg[wordStart:i]...), false
			}
			i++
			continue
		}
		if quote != 0 {
			i, quote = unquoteStep(&buf, seg, i, quote)
			started = true
			continue
		}
		if isTokenBoundary(seg[i]) {
			if started {
				out = appendToken(out, seg, wordStart, i, raw, buf)
				buf, started, raw = buf[:0], false, true
			}
			continue
		}
		if !started {
			started, raw, wordStart = true, true, i
		}
		i, quote, raw = plainTokenStep(&buf, seg, i, wordStart, raw)
	}
	if started {
		out = appendToken(out, seg, wordStart, len(seg), raw, buf)
	}
	return trimGroupOpen(out)
}

func isTokenBoundary(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ')' || c == '}'
}

func appendToken(out []string, seg string, start, end int, raw bool, buf []byte) []string {
	if raw {
		return append(out, seg[start:end])
	}
	return append(out, string(buf))
}

// lineContinuation reports a backslash-newline at seg[i] outside single quotes.
func lineContinuation(seg string, i int, quote byte) bool {
	return seg[i] == '\\' && quote != '\'' && i+1 < len(seg) && seg[i+1] == '\n'
}

// dollarBraceEnd is braceExpansionEnd when seg[i] is `$`, else -1.
func dollarBraceEnd(seg string, i int) int {
	if seg[i] != '$' {
		return -1
	}
	return braceExpansionEnd(seg, i)
}

// braceExpansionEnd is the index of the `}` closing a `${` at seg[i], or -1 when seg[i:] opens none.
func braceExpansionEnd(seg string, i int) int {
	if !braceAt(seg, i+1) {
		return -1
	}
	depth := 1
	for j := i + 2; j < len(seg); j++ {
		switch seg[j] {
		case '\\':
			j++
		case '\'', '"':
			k := quotedEnd(seg, j)
			if k < 0 {
				return -1
			}
			j = k
		case '$':
			if braceAt(seg, j+1) {
				depth++
				j++
			}
		case '}':
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

// braceAt reports a `{` at seg[i].
func braceAt(seg string, i int) bool { return i < len(seg) && seg[i] == '{' }

// quotedEnd is the index of the quote closing the one opened at seg[i], or -1 when unterminated.
func quotedEnd(seg string, i int) int {
	quote := seg[i]
	for j := i + 1; j < len(seg); j++ {
		var q byte
		if j, q = quotedStep(seg, j, quote); q == 0 {
			return j
		}
	}
	return -1
}

// plainTokenStep handles an unquoted, non-boundary byte seg[i], switching to buf only on quotes/escapes.
func plainTokenStep(buf *[]byte, seg string, i, wordStart int, raw bool) (int, byte, bool) {
	if end := dollarBraceEnd(seg, i); end > 0 {
		if !raw {
			*buf = append(*buf, seg[i:end+1]...)
		}
		return end, 0, raw
	}
	switch c := seg[i]; {
	case c == '$' && i+1 < len(seg) && seg[i+1] == '\'':
		if raw {
			*buf = append((*buf)[:0], seg[wordStart:i]...)
		}
		return decodeANSIC(buf, seg, i+2), 0, false
	case c == '\'' || c == '"':
		if raw {
			*buf = append((*buf)[:0], seg[wordStart:i]...)
		}
		return i, c, false
	case c == '\\' && i+1 < len(seg):
		if raw {
			*buf = append((*buf)[:0], seg[wordStart:i]...)
		}
		return i + 1, 0, appendByte(buf, seg[i+1])
	default:
		if !raw {
			*buf = append(*buf, c)
		}
		return i, 0, raw
	}
}

func appendByte(buf *[]byte, b byte) bool {
	*buf = append(*buf, b)
	return false
}

// unquoteStep writes quoted byte seg[i] to buf, resolving a double-quote escape and dropping the closing quote.
func unquoteStep(buf *[]byte, seg string, i int, quote byte) (int, byte) {
	if quote == '"' && seg[i] == '\\' && i+1 < len(seg) && dqEscapable(seg[i+1]) {
		*buf = append(*buf, seg[i+1])
		return i + 1, quote
	}
	if seg[i] == quote {
		return i, 0
	}
	*buf = append(*buf, seg[i])
	return i, quote
}

// trimGroupOpen drops `(`/`{` openers from the leading words (also after control words), and words left empty.
func trimGroupOpen(out []string) []string {
	for i := 0; i < len(out); {
		if trimmed := strings.TrimLeft(out[i], "({"); trimmed != out[i] {
			out[i] = trimmed
			if trimmed == "" {
				out = slices.Delete(out, i, i+1)
				continue
			}
		}
		if !isShellKeyword(out[i]) {
			break
		}
		i++
	}
	return out
}

func isShellKeyword(s string) bool { walkerOnce.Do(loadWalker); return walker.keywords[s] }

// walker holds the walker's per-call word sets; its accessors inline where a sync.OnceValue call does not.
var (
	walkerOnce sync.Once
	walker     walkerSets
)

type walkerSets struct {
	keywords, shells, wrappers, operands, gitValueOpts, gitBuiltins            map[string]bool
	pureReaders, evaluators, runArgs, inert, inertHarness, rtkRunners, cString map[string]bool
	wrapperValueFlags                                                          map[string]map[string]bool
	execFlags, runnerValueFlags, goRunValueFlags                               []string
}

func loadWalker() {
	walker = walkerSets{
		keywords: GuardSet("shell_keywords"), shells: GuardSet("shell_names"), wrappers: GuardSet("wrapper_names"),
		operands: GuardSet("wrapper_operand_commands"), gitValueOpts: GuardSet("git_global_value_flags"),
		gitBuiltins: GuardSet("git_alias_proof_verbs"), wrapperValueFlags: GuardToolSets("wrapper_value_flags"),
		pureReaders: GuardSet("pure_readers"), evaluators: GuardSet("evaluators"), runArgs: GuardSet("run_arg_commands"),
		inert: GuardSet("inert_commands"), inertHarness: GuardSet("inert_harness_verbs"),
		rtkRunners: GuardSet("rtk_runner_subcommands"), cString: GuardSet("c_string_commands"),
		execFlags: GuardWords("find_exec_flags"), runnerValueFlags: GuardWords("runner_value_flags"),
		goRunValueFlags: GuardWords("go_run_value_flags"),
	}
}

// quotedStep advances over s[i] inside quote: a double-quote escape consumes the next byte, the closing quote ends it.
func quotedStep(s string, i int, quote byte) (int, byte) {
	if quote == ansiQuote { // inside $'…' a backslash escapes any byte, including the quote
		if s[i] == '\\' && i+1 < len(s) {
			return i + 1, quote
		}
		if s[i] == '\'' {
			return i, 0
		}
		return i, quote
	}
	if s[i] == '\\' && quote == '"' && i+1 < len(s) && dqEscapable(s[i+1]) {
		return i + 1, quote
	}
	if s[i] == quote {
		return i, 0
	}
	return i, quote
}

// ansiQuote is the quote state of an open ANSI-C `$'…'` string.
const ansiQuote = 1

// dqEscapable are the bytes a backslash escapes inside POSIX double quotes; elsewhere it is literal.
func dqEscapable(c byte) bool {
	return c == '"' || c == '\\' || c == '$' || c == '`'
}

// ExtractSubstitutions pulls $(...), backtick and unquoted <(...)/>(...) bodies out of cmd, leaving a space each.
func ExtractSubstitutions(s string) (string, []string) { return extractSubstitutions(s, " ") }

// subMask replaces a substitution where a caller must tell "a word was here" from nothing (git's verb position).
const subMask = "\x00"

// CommandWords is each simple command's Tokens, every `$(…)`, backtick or `<(…)` kept unevaluated in its word.
func CommandWords(cmd string) [][]string {
	masked, subs := extractSubstitutions(cmd, subMask)
	if strings.Count(masked, subMask) != len(subs) { // a NUL already in cmd: the masks cannot be told apart
		masked, subs = cmd, nil
	}
	texts := subTexts(cmd, masked, subs)
	segs := Segments(masked)
	out := make([][]string, len(segs))
	for i, seg := range segs {
		out[i] = Tokens(seg)
		for j, w := range out[i] {
			for len(texts) > 0 && strings.Contains(w, subMask) {
				w, texts = strings.Replace(w, subMask, texts[0], 1), texts[1:]
			}
			out[i][j] = w
		}
	}
	return out
}

// subTexts is each substitution's source text in cmd, found where masked holds one subMask byte in its place.
func subTexts(cmd, masked string, subs []string) []string {
	texts := make([]string, len(subs))
	shift, from := 0, 0
	for k, body := range subs {
		at := from + strings.Index(masked[from:], subMask)
		n := len(body) + len("$()")
		if cmd[at+shift] == '`' {
			n = len(body) + len("``")
		}
		texts[k] = cmd[at+shift : at+shift+n]
		shift, from = shift+n-1, at+1
	}
	return texts
}

func extractSubstitutions(s, mask string) (string, []string) {
	var out strings.Builder
	var subs []string
	var quote byte
	copied := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case escapedAt(s, i, quote):
			i++
		case c == '\'' && quote == 0: // inside "…" a ' is literal
			quote = '\''
		case c == '"':
			quote = toggleDoubleQuote(quote)
		default:
			if body, end, ok := substitutionAt(s, i, quote); ok {
				if copied == 0 {
					out.Grow(len(s))
				}
				out.WriteString(s[copied:i])
				out.WriteString(mask)
				subs = append(subs, body)
				i, copied = end, end+1
			}
		}
	}
	if copied == 0 {
		return s, nil
	}
	out.WriteString(s[copied:])
	return out.String(), subs
}

// escapedAt reports a backslash at s[i] that escapes the next byte (always outside quotes, dqEscapable inside).
func escapedAt(s string, i int, quote byte) bool {
	return s[i] == '\\' && i+1 < len(s) && (quote == 0 || dqEscapable(s[i+1]))
}

// toggleDoubleQuote opens a double quote outside quotes and closes an open one.
func toggleDoubleQuote(quote byte) byte {
	if quote == '"' {
		return 0
	}
	return '"'
}

// substitutionAt returns the body and close of a `$(…)`, backtick or unquoted `<(…)`/`>(…)` at s[i].
func substitutionAt(s string, i int, quote byte) (string, int, bool) {
	switch {
	case (s[i] == '$' || quote == 0 && (s[i] == '<' || s[i] == '>')) && i+1 < len(s) && s[i+1] == '(':
		if end := matchParen(s, i+1); end >= 0 {
			return s[i+2 : end], end, true
		}
	case s[i] == '`':
		if end := matchBacktick(s, i); end >= 0 {
			return s[i+1 : end], end, true
		}
	}
	return "", i, false
}

// matchParen returns the index of the `)` matching the `(` at s[open], or -1; nested parens/quotes count.
func matchParen(s string, open int) int {
	depth, quote := 0, byte(0)
	for i := open; i < len(s); i++ {
		if quote != 0 {
			i, quote = quotedStep(s, i, quote)
			continue
		}
		if next, q, ok := openQuote(s, i); ok {
			i, quote = next, q
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// matchBacktick returns the index of the closing backtick matching s[start], or -1; quotes inside count.
func matchBacktick(s string, start int) int {
	quote := byte(0)
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			i, quote = quotedStep(s, i, quote)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '\\':
			if i+1 < len(s) {
				i++
			}
		case '`':
			return i
		}
	}
	return -1
}

// shellWrapperNames launch a nested command STRING (not further argv tokens) that needs its own check.
func shellWrapperNames() map[string]bool { walkerOnce.Do(loadWalker); return walker.shells }

// isEnvAssign reports a leading `NAME=value` token (e.g. `BASH_ENV=x`) prefixed to a command.
func isEnvAssign(t string) bool {
	eq := strings.IndexByte(t, '=')
	if eq <= 0 {
		return false
	}
	c0 := t[0]
	if (c0 < 'A' || c0 > 'Z') && (c0 < 'a' || c0 > 'z') && c0 != '_' {
		return false
	}
	for i := 1; i < eq; i++ {
		c := t[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// skipEnvAssignments drops leading `NAME=value` tokens so the wrapper/eval check sees the real command.
func skipEnvAssignments(toks []string) []string {
	i := 0
	for i < len(toks) && isEnvAssign(toks[i]) {
		i++
	}
	return toks[i:]
}

// wrapperValueFlags are a prefix wrapper's flags that take the next token (long forms also as --opt=val).
func wrapperValueFlags() map[string]map[string]bool {
	walkerOnce.Do(loadWalker)
	return walker.wrapperValueFlags
}

// wrapperOperands are the positional operands a wrapper takes before its command (timeout's duration, chrt's priority).
func wrapperOperands() map[string]bool { walkerOnce.Do(loadWalker); return walker.operands }

// wrapperNames are the prefix wrappers Unwrap sees through.
func wrapperNames() map[string]bool { walkerOnce.Do(loadWalker); return walker.wrappers }

// Unwrap drops leading NAME=val and wrappers (env -S split); dir is cwd moved by `env -C` or `sudo -D`.
func Unwrap(toks []string, cwd string) (rest []string, dir string) {
	dir = cwd
	for {
		toks = skipEnvAssignments(toks)
		if len(toks) == 0 || !wrapperNames()[filepath.Base(toks[0])] {
			return toks, dir
		}
		name := filepath.Base(toks[0])
		toks, dir = skipWrapperOpts(name, toks[1:], dir)
		if wrapperOperands()[name] && len(toks) > 0 { // the duration or priority operand
			toks = toks[1:]
		}
	}
}

// skipWrapperOpts consumes one wrapper's options, tracking a chdir option and env -S.
func skipWrapperOpts(name string, toks []string, dir string) ([]string, string) {
	for len(toks) > 0 && strings.HasPrefix(toks[0], "-") && toks[0] != "-" {
		opt := toks[0]
		toks = toks[1:]
		if opt == "--" {
			break
		}
		flag, val, hasVal := splitWrapperOpt(name, opt)
		if !wrapperValueFlags()[name][flag] {
			continue
		}
		if !hasVal {
			if len(toks) == 0 {
				break
			}
			val, toks = toks[0], toks[1:]
		}
		toks, dir = applyWrapperValue(name, flag, val, toks, dir)
	}
	return toks, dir
}

// splitWrapperOpt splits `--opt=val`, an attached short value (`-Sstr`, `-C/dir`) and a cluster ending in one (`-nu`).
func splitWrapperOpt(name, opt string) (flag, val string, hasVal bool) {
	if strings.HasPrefix(opt, "--") {
		return strings.Cut(opt, "=")
	}
	flags := wrapperValueFlags()[name]
	var key [2]byte
	key[0] = '-'
	for i := 1; i < len(opt); i++ {
		key[1] = opt[i]
		if flags[string(key[:])] {
			return "-" + opt[i:i+1], opt[i+1:], i+1 < len(opt)
		}
	}
	return opt, "", false
}

// applyWrapperValue acts on a value option: env -S splits its string into the command line, a chdir option moves dir.
func applyWrapperValue(name, flag, val string, toks []string, dir string) ([]string, string) {
	switch {
	case name == "env" && (flag == "-S" || flag == "--split-string"):
		toks = append(Tokens(val), toks...)
	case flag == "--chdir" || (name == "env" && flag == "-C") || (name == "sudo" && flag == "-D"):
		if p, ok := resolvePath(val, dir); ok {
			dir = p
		}
	}
	return toks, dir
}

// NestedCommand reports the command text a `bash -c "…"`/`eval "…"` token sequence would run.
func NestedCommand(toks []string) (string, bool) {
	rest, _ := Unwrap(toks, "")
	return nestedOf(rest)
}

// nestedOf is NestedCommand for tokens whose wrappers are already dropped.
func nestedOf(toks []string) (string, bool) {
	if len(toks) == 0 {
		return "", false
	}
	if toks[0] == "eval" && len(toks) > 1 {
		return strings.Join(toks[1:], " "), true
	}
	if shellWrapperNames()[filepath.Base(toks[0])] {
		for i := 1; i < len(toks)-1; i++ {
			if isCFlag(toks[i]) {
				return toks[i+1], true
			}
		}
	}
	return "", false
}

// CdTarget reports the cwd a `cd`/`pushd` command leaves behind; a dynamic target keeps the cwd we had.
func CdTarget(toks []string, cwd string) (string, bool) {
	next, _, ok := cdResolve(toks, cwd)
	return next, ok
}

// cdResolve is CdTarget plus whether the target is dynamic (a `$VAR`, `-`, substitution or glob).
func cdResolve(toks []string, cwd string) (next string, dynamic, ok bool) {
	if len(toks) == 0 || toks[0] != "cd" && toks[0] != "pushd" {
		return cwd, false, false
	}
	args := skipCdOptions(toks[1:])
	if len(args) == 0 {
		if home, err := os.UserHomeDir(); err == nil && toks[0] == "cd" {
			return home, false, true
		}
		return cwd, false, true
	}
	if p, ok := resolvePath(args[0], cwd); ok {
		return p, false, true
	}
	return cwd, true, true
}

// skipCdOptions drops cd's leading options (-L -P -e -@) and a `--` terminator.
func skipCdOptions(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		opt := args[0]
		args = args[1:]
		if opt == "--" {
			break
		}
	}
	return args
}

// resolvePath expands `~`/$HOME and anchors p to base; ok=false while the token stays dynamic.
func resolvePath(p, base string) (string, bool) {
	p = strings.Trim(p, `'"`)
	if p == "" || p == "-" {
		return base, false
	}
	if p[0] == '~' || p[0] == '$' {
		if home, err := os.UserHomeDir(); err == nil {
			p = expandHomePrefix(p, home)
		}
	}
	if strings.ContainsAny(p, "$`*?"+subMask) || p[0] == '~' { // ~user, ~+, ~- name other dirs
		return base, false
	}
	if base == unknownDir && !filepath.IsAbs(p) { // a `..` must not climb out of an unknown dir into a real one
		return unknownDir, true
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return p, true
}

func expandHomePrefix(p, home string) string {
	switch {
	case p == "~", strings.HasPrefix(p, "~/"):
		return home + p[1:]
	case p == "$HOME", strings.HasPrefix(p, "$HOME/"):
		return home + p[len("$HOME"):]
	case p == "${HOME}", strings.HasPrefix(p, "${HOME}/"):
		return home + p[len("${HOME}"):]
	}
	return p
}

// decodeANSIC writes the body of an ANSI-C `$'…'` string from seg[i] to buf; returns the closing quote index.
func decodeANSIC(buf *[]byte, seg string, i int) int {
	for ; i < len(seg) && seg[i] != '\''; i++ {
		if seg[i] != '\\' || i+1 >= len(seg) {
			*buf = append(*buf, seg[i])
			continue
		}
		i++
		if r, n, ok := ansiEscape(seg[i:]); ok {
			*buf = append(*buf, r...)
			i += n - 1
		} else {
			*buf = append(*buf, '\\', seg[i])
		}
	}
	return i
}

var ansiSimple = map[byte]string{'n': "\n", 't': "\t", 'r': "\r", 'a': "\a", 'b': "\b", 'e': "\x1b", 'E': "\x1b",
	'f': "\f", 'v': "\v", '\\': "\\", '\'': "'", '"': `"`, '?': "?"}

// ansiEscape decodes one escape (text after the backslash: simple, \xHH, \u, \U, octal); n is bytes used.
func ansiEscape(s string) (out string, n int, ok bool) {
	if r, found := ansiSimple[s[0]]; found {
		return r, 1, true
	}
	maxDigits, base, bits := 0, 16, 8
	switch c := s[0]; {
	case c == 'x':
		maxDigits = 2
	case c == 'u':
		maxDigits, bits = 4, 32
	case c == 'U':
		maxDigits, bits = 8, 32
	case c >= '0' && c <= '7':
		maxDigits, base = 3, 8
	default:
		return "", 0, false
	}
	start := 0
	if base == 16 {
		start = 1
	}
	end := digitRun(s, start, maxDigits, base)
	if end == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseUint(s[start:end], base, 32)
	if err != nil {
		return "", 0, false
	}
	if bits == 8 {
		return string([]byte{byte(v)}), end, true
	}
	return string(rune(v)), end, true
}

// digitRun is the end of the run of up to maxDigits base-8 or base-16 digits at s[start:], or 0 when there are none.
func digitRun(s string, start, maxDigits, base int) int {
	digits := "01234567"
	if base == 16 {
		digits = "0123456789abcdefABCDEF"
	}
	end := start
	for end < len(s) && end-start < maxDigits && strings.IndexByte(digits, s[end]) >= 0 {
		end++
	}
	if end == start {
		return 0
	}
	return end
}
