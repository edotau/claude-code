package shell

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

var footerLine = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)^\W*(co-authored-by:|generated (with|by))`)
})

// messageArg is ` -m "text"` / ` --message 'text'` with a literal body: no escape, `$` or backtick in double quotes.
var messageArg = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile("^[ \t]+(?:-m|--message)[ \t]+(\"[^\"\\\\$`]*\"|'[^']*')")
})

// simpleCommands reports plain commands only: no heredoc, substitution, grouping, comment, continuation or keyword.
func simpleCommands(cmd string) bool {
	if strings.ContainsAny(cmd, "`\r") || strings.Contains(cmd, "<<") || strings.Contains(cmd, "$(") || strings.Contains(cmd, "\\\n") {
		return false
	}
	if !plainUnquoted(cmd) {
		return false
	}
	segs, _ := SegmentsSep(cmd)
	return !slices.ContainsFunc(segs, func(s string) bool {
		t := Tokens(s)
		return len(t) > 0 && slices.Contains(shellKeywords, t[0])
	})
}

// plainUnquoted is false when an unquoted byte of cmd is a grouping bracket or starts a comment.
func plainUnquoted(cmd string) bool {
	q := QuoteAt(cmd)
	for i := 0; i < len(cmd); i++ {
		if q[i] != 0 {
			continue
		}
		if strings.IndexByte("(){}", cmd[i]) >= 0 || cmd[i] == '#' && (i == 0 || strings.IndexByte(" \t\n;&|", cmd[i-1]) >= 0) {
			return false
		}
	}
	return true
}

// ChainPushAfterGate turns `;`/newline between a gate and `git push` into `&&`; ok is false unless commands are simple.
func ChainPushAfterGate(cmd string) (fixed, gate string, ok bool) {
	if !simpleCommands(cmd) {
		return "", "", false
	}
	segs, seps := SegmentsSep(cmd)
	var b strings.Builder
	b.WriteString(segs[0])
	for i := 0; i+1 < len(segs); i++ {
		if seps[i] == ";" || seps[i] == "\n" {
			if found, direct := gateRun(segs, seps, i, i+1); found {
				if !direct || i > 0 && seps[i-1] == "||" {
					return "", "", false // && after a pipeline or `||` would not test the gate itself
				}
				if gate == "" {
					gate = strings.TrimSpace(segs[i])
				}
				chained := strings.TrimRight(b.String(), " \t") + " && " + strings.TrimLeft(segs[i+1], " \t")
				b.Reset()
				b.WriteString(chained)
				continue
			}
		}
		b.WriteString(seps[i] + segs[i+1])
	}
	return b.String(), gate, gate != ""
}

// StripFooters drops footer lines from literal -m arguments of a simple `git commit`; ok is false for any other shape.
func StripFooters(cmd string) (string, bool) {
	if !simpleCommands(cmd) {
		return cmd, false
	}
	segs, seps := SegmentsSep(cmd)
	var b strings.Builder
	for i, s := range segs {
		if isCommitSegment(s) {
			s = stripSegmentFooters(s)
		}
		b.WriteString(s + seps[i])
	}
	fixed := b.String()
	return fixed, fixed != cmd && !hasFooter(fixed)
}

func isCommitSegment(seg string) bool {
	toks, _ := Unwrap(builtinPrefixDropped(commandWords(seg)), "")
	if len(toks) == 0 || toks[0] != "git" {
		return false
	}
	_, verb, _ := GitInvocation(toks[1:], "")
	return verb == "commit"
}

// stripSegmentFooters cleans each message argument outside other quotes, leaving every other byte as written.
func stripSegmentFooters(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == ' ' || c == '\t':
			if m := messageArg().FindStringSubmatchIndex(s[i:]); m != nil && wordEnd(s, i+m[1]) {
				out.WriteString(cleanMessageArg(s[i:i+m[2]], s[i+m[2]:i+m[3]]))
				i += m[1]
				continue
			}
		case c == '"' || c == '\'':
			j := quoteEnd(s, i)
			out.WriteString(s[i:j])
			i = j
			continue
		case c == '\\' && i+1 < len(s):
			out.WriteByte(c)
			i++
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

func wordEnd(s string, i int) bool { return i >= len(s) || s[i] == ' ' || s[i] == '\t' }

// quoteEnd is the index just past the quote opened at s[i] (the end of s when unterminated).
func quoteEnd(s string, i int) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' && q == '"' {
			j++
		} else if s[j] == q {
			return j + 1
		}
	}
	return len(s)
}

// cleanMessageArg blanks footer-only lines of one quoted message; the whole argument goes when only footers were in it.
func cleanMessageArg(prefix, quoted string) string {
	lines := strings.Split(quoted[1:len(quoted)-1], "\n")
	changed, kept := false, false
	for i, l := range lines {
		switch {
		case !hasFooter(l):
			kept = kept || strings.TrimSpace(l) != ""
		case footerLine().MatchString(strings.TrimSpace(l)):
			lines[i], changed = "", true
		default:
			return prefix + quoted // the footer shares its line with other text
		}
	}
	if !changed {
		return prefix + quoted
	}
	if !kept {
		return ""
	}
	return prefix + quoted[:1] + strings.Join(lines, "\n") + quoted[len(quoted)-1:]
}
