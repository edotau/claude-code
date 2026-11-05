// Package shell parses shell command text for hooks: segments, words, substitutions, wrappers and git invocations.
package shell

import (
	"os"
	"path/filepath"
	"regexp"
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
		if quote != 0 {
			i, quote = unquoteStep(&b, seg, i, quote)
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
		case c == '\\' && i+1 < len(seg):
			i++
			b.WriteByte(seg[i])
		case strings.IndexByte(" \t\r\n)}", c) >= 0:
			flush() // whitespace, or an unquoted close of a subshell/group: a word boundary, never part of the word
			continue
		default:
			b.WriteByte(c)
		}
		started = true
	}
	flush()
	return trimGroupOpen(out)
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

// trimGroupOpen drops a leading `(`/`{` from the first word, and the word itself when nothing else is left.
func trimGroupOpen(out []string) []string {
	if len(out) > 0 {
		out[0] = strings.TrimLeft(out[0], "({")
		if out[0] == "" {
			out = out[1:]
		}
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
func ExtractSubstitutions(s string) (string, []string) {
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
				c = ' '
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

// wrapperValueFlags are the option flags of a prefix wrapper that consume the next token.
var wrapperValueFlags = map[string]map[string]bool{
	"sudo": {"-u": true, "-g": true, "-h": true, "-p": true, "-C": true, "-D": true, "-R": true, "-T": true, "-U": true},
	"env":  {"-u": true, "-C": true, "-S": true},
	"nice": {"-n": true},
}

// skipPrefixWrappers drops leading env/sudo/timeout/nohup/nice/command/exec/time wrappers and their options.
func skipPrefixWrappers(toks []string) []string {
	for {
		toks = skipEnvAssignments(toks)
		if len(toks) == 0 {
			return toks
		}
		name := filepath.Base(toks[0])
		switch name {
		case "env", "sudo", "nice", "nohup", "command", "exec", "time", "timeout":
		default:
			return toks
		}
		toks = toks[1:]
		for len(toks) > 0 && strings.HasPrefix(toks[0], "-") && toks[0] != "-" {
			opt := toks[0]
			toks = toks[1:]
			if opt == "--" {
				break
			}
			if wrapperValueFlags[name][opt] && len(toks) > 0 {
				toks = toks[1:]
			}
		}
		if name == "timeout" && len(toks) > 0 { // the duration operand
			toks = toks[1:]
		}
	}
}

// NestedCommand reports the command text a `bash -c "…"`/`eval "…"` token sequence would run.
func NestedCommand(toks []string) (string, bool) {
	toks = skipPrefixWrappers(toks)
	if len(toks) == 0 {
		return "", false
	}
	if toks[0] == "eval" && len(toks) > 1 {
		return strings.Join(toks[1:], " "), true
	}
	if shellWrapperNames[filepath.Base(toks[0])] {
		for i := 1; i < len(toks)-1; i++ {
			t := toks[i]
			if len(t) > 1 && t[0] == '-' && t[1] != '-' && strings.ContainsRune(t[1:], 'c') {
				return toks[i+1], true
			}
		}
	}
	return "", false
}

// ResolvePath expands `~`/$HOME and anchors p to base; ok=false while the token stays dynamic.
func ResolvePath(p, base string) (string, bool) {
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
	if strings.ContainsAny(p, "$`*?") {
		return base, false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return p, true
}
