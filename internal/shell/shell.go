// Package shell parses shell command text for hooks: segments, words, substitutions, wrappers and git invocations.
package shell

import (
	"os"
	"path/filepath"
	"regexp"
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
	var b strings.Builder
	var quote byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			j, q := quotedStep(cmd, i, quote)
			b.WriteString(cmd[i : j+1])
			i, quote = j, q
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
		case c == '\\' && i+1 < len(cmd):
			b.WriteByte(c)
			i++
			c = cmd[i]
		case c == '&' && redirectAmp(cmd, i):
			// `2>&1`, `<&3` and `&>file` are redirections, not backgrounding
		case strings.IndexByte(";|&\n", c) >= 0:
			sep, end := separatorAt(cmd, i)
			out, seps = append(out, b.String()), append(seps, sep)
			b.Reset()
			i = end
			continue
		}
		b.WriteByte(c)
	}
	return append(out, b.String()), append(seps, "")
}

// redirectAmp reports a `&` that follows `>`/`<` or precedes `>`, a redirection rather than a separator.
func redirectAmp(cmd string, i int) bool {
	return i > 0 && (cmd[i-1] == '>' || cmd[i-1] == '<') || i+1 < len(cmd) && cmd[i+1] == '>'
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
	var b strings.Builder
	var quote byte
	started := false
	flush := func() {
		if started {
			out = append(out, b.String())
			b.Reset()
			started = false
		}
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c == '\\' && quote != '\'' && i+1 < len(seg) && seg[i+1] == '\n' {
			i++ // backslash-newline is a line continuation: removed, so the words on either side join
			continue
		}
		switch {
		case quote != 0:
			i, quote = unquoteStep(&b, seg, i, quote)
		case strings.IndexByte(" \t\r\n)}", c) >= 0:
			flush() // whitespace, or an unquoted close of a subshell/group: a word boundary, never part of the word
			continue
		default:
			if end := dollarBraceEnd(seg, i); end > 0 {
				b.WriteString(seg[i : end+1]) // a `${VAR}` expansion stays one word
				i = end
			} else {
				i, quote = plainStep(&b, seg, i)
			}
		}
		started = true
	}
	flush()
	return trimGroupOpen(out)
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
	if i+1 >= len(seg) || seg[i+1] != '{' {
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
			if j+1 < len(seg) && seg[j+1] == '{' {
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

// plainStep handles an unquoted, non-boundary byte seg[i]: opens a quote, resolves a backslash escape or writes it.
func plainStep(b *strings.Builder, seg string, i int) (int, byte) {
	c := seg[i]
	switch {
	case c == '$' && i+1 < len(seg) && seg[i+1] == '\'':
		end := decodeANSIC(b, seg, i+2)
		return end, 0
	case c == '\'' || c == '"':
		return i, c
	case c == '\\' && i+1 < len(seg):
		i++
		b.WriteByte(seg[i])
	default:
		b.WriteByte(c)
	}
	return i, 0
}

// unquoteStep writes quoted byte seg[i] to b, resolving a double-quote escape and dropping the closing quote.
func unquoteStep(b *strings.Builder, seg string, i int, quote byte) (int, byte) {
	if quote == '"' && seg[i] == '\\' && i+1 < len(seg) && dqEscapable(seg[i+1]) {
		b.WriteByte(seg[i+1])
		return i + 1, quote
	}
	if seg[i] == quote {
		return i, 0
	}
	b.WriteByte(seg[i])
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
		if !slices.Contains(shellKeywords, out[i]) {
			break
		}
		i++
	}
	return out
}

// quotedStep advances over s[i] inside quote: a double-quote escape consumes the next byte, the closing quote ends it.
func quotedStep(s string, i int, quote byte) (int, byte) {
	if s[i] == '\\' && quote == '"' && i+1 < len(s) && dqEscapable(s[i+1]) {
		return i + 1, quote
	}
	if s[i] == quote {
		return i, 0
	}
	return i, quote
}

// dqEscapable are the bytes a backslash escapes inside POSIX double quotes; elsewhere it is literal.
func dqEscapable(c byte) bool {
	return c == '"' || c == '\\' || c == '$' || c == '`'
}

// ExtractSubstitutions pulls `$(...)` and backtick command substitutions out of cmd — including inside
// double quotes, where the shell evaluates both — leaving a placeholder space so neighboring tokens don't
// glue; single-quoted text is literal, so a substitution there is left untouched. Returns the bodies for
// the caller to check separately: they run as their own nested command, not inline in the outer segment.
func ExtractSubstitutions(s string) (string, []string) { return extractSubstitutions(s, " ") }

// subMask replaces a substitution where a caller must tell "a word was here" from nothing (git's verb position).
const subMask = "\x00"

func extractSubstitutions(s, mask string) (string, []string) {
	var out strings.Builder
	var subs []string
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case escapedAt(s, i, quote):
			out.WriteByte(c)
			i++
			c = s[i]
		case c == '\'' && quote == 0: // inside "…" a ' is literal
			quote = '\''
		case c == '"':
			quote = toggleDoubleQuote(quote)
		default:
			if body, end, ok := substitutionAt(s, i); ok {
				subs = append(subs, body)
				i = end
				out.WriteString(mask)
				continue
			}
		}
		out.WriteByte(c)
	}
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

// substitutionAt reports a complete `$(…)` or backtick substitution starting at s[i]: its body and closing index.
func substitutionAt(s string, i int) (string, int, bool) {
	switch {
	case s[i] == '$' && i+1 < len(s) && s[i+1] == '(':
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
		c := s[i]
		if quote != 0 {
			i, quote = quotedStep(s, i, quote)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
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
var shellWrapperNames = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true}

// envAssignRe matches a leading `NAME=value` token (e.g. `BASH_ENV=x`) prefixed to a command.
var envAssignRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
})

// skipEnvAssignments drops leading `NAME=value` tokens so the wrapper/eval check sees the real command.
func skipEnvAssignments(toks []string) []string {
	i := 0
	for i < len(toks) && envAssignRe().MatchString(toks[i]) {
		i++
	}
	return toks[i:]
}

// wrapperValueFlags are the option flags of a prefix wrapper that consume the next token (long forms also as --opt=val).
var wrapperValueFlags = map[string]map[string]bool{
	"sudo": {"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-D": true, "-R": true, "-T": true, "-U": true,
		"-r": true, "-t": true, "--user": true, "--group": true, "--host": true, "--prompt": true, "--close-from": true,
		"--chdir": true, "--role": true, "--type": true, "--other-user": true},
	"env":     {"-u": true, "-C": true, "-S": true, "--unset": true, "--chdir": true, "--split-string": true},
	"nice":    {"-n": true, "--adjustment": true},
	"timeout": {"-s": true, "--signal": true, "-k": true, "--kill-after": true},
	"exec":    {"-a": true},
	"stdbuf":  {"-i": true, "-o": true, "-e": true, "--input": true, "--output": true, "--error": true},
	"ionice":  {"-c": true, "-n": true, "-p": true, "-P": true, "-u": true},
}

// wrapperOperands are the positional operands a wrapper takes before its command (timeout's duration, chrt's priority).
var wrapperOperands = map[string]bool{"timeout": true, "chrt": true}

// wrapperNames are the prefix wrappers Unwrap sees through.
var wrapperNames = map[string]bool{"env": true, "sudo": true, "nice": true, "nohup": true, "command": true,
	"exec": true, "time": true, "timeout": true, "setsid": true, "stdbuf": true, "ionice": true, "chrt": true}

// Unwrap drops leading NAME=val words and env/sudo/timeout/nohup/nice/command/exec/time wrappers with their options.
// dir is cwd moved by `env -C/--chdir` and `sudo -D/--chdir`; an `env -S STR` string is split into the command line.
func Unwrap(toks []string, cwd string) (rest []string, dir string) {
	dir = cwd
	for {
		toks = skipEnvAssignments(toks)
		if len(toks) == 0 || !wrapperNames[filepath.Base(toks[0])] {
			return toks, dir
		}
		name := filepath.Base(toks[0])
		toks, dir = skipWrapperOpts(name, toks[1:], dir)
		if wrapperOperands[name] && len(toks) > 0 { // the duration or priority operand
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
		if !wrapperValueFlags[name][flag] {
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
	for i := 1; i < len(opt); i++ {
		if f := "-" + opt[i:i+1]; wrapperValueFlags[name][f] {
			return f, opt[i+1:], i+1 < len(opt)
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
	if shellWrapperNames[filepath.Base(toks[0])] {
		for i := 1; i < len(toks)-1; i++ {
			t := toks[i]
			if isCFlag(t) {
				return toks[i+1], true
			}
		}
	}
	return "", false
}

// CdTarget reports the cwd a `cd`/`pushd` command leaves behind; a dynamic target keeps the cwd we had.
func CdTarget(toks []string, cwd string) (string, bool) {
	if len(toks) == 0 || toks[0] != "cd" && toks[0] != "pushd" {
		return cwd, false
	}
	args := skipCdOptions(toks[1:])
	if len(args) == 0 {
		if home, err := os.UserHomeDir(); err == nil && toks[0] == "cd" {
			return home, true
		}
		return cwd, true
	}
	if p, ok := resolvePath(args[0], cwd); ok {
		return p, true
	}
	return cwd, true
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
	if home, err := os.UserHomeDir(); err == nil {
		for _, pre := range []string{"~", "$HOME", "${HOME}"} {
			if p == pre || strings.HasPrefix(p, pre+"/") {
				p = home + p[len(pre):]
				break
			}
		}
	}
	if strings.ContainsAny(p, "$`*?"+subMask) {
		return base, false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return p, true
}

// decodeANSIC writes the body of an ANSI-C `$'…'` string from seg[i] to b; returns the closing quote index.
func decodeANSIC(b *strings.Builder, seg string, i int) int {
	for ; i < len(seg) && seg[i] != '\''; i++ {
		if seg[i] != '\\' || i+1 >= len(seg) {
			b.WriteByte(seg[i])
			continue
		}
		i++
		if r, n, ok := ansiEscape(seg[i:]); ok {
			b.WriteString(r)
			i += n - 1
		} else {
			b.WriteByte('\\')
			b.WriteByte(seg[i])
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
	digits, base, bits := 0, 16, 8
	switch c := s[0]; {
	case c == 'x':
		digits = 2
	case c == 'u':
		digits, bits = 4, 32
	case c == 'U':
		digits, bits = 8, 32
	case c >= '0' && c <= '7':
		digits, base = 3, 8
	default:
		return "", 0, false
	}
	start := 0
	if base == 16 {
		start = 1
	}
	end := min(start+digits, len(s))
	v, err := strconv.ParseUint(s[start:end], base, 32)
	if err != nil {
		return "", 0, false
	}
	if bits == 8 {
		return string([]byte{byte(v)}), end, true
	}
	return string(rune(v)), end, true
}
