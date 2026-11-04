package hooks

import (
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
)

// heredocOp matches one `<<[-]DELIM` operator; groups: dash, single-quoted, double-quoted, bare delimiter.
var heredocOp = regexp.MustCompile(`<<(-?)\s*(?:'([^']*)'|"([^"]*)"|([A-Za-z_][A-Za-z0-9_]*))`)

// fdToSubst matches a redirect to fd 3+, which `exec 3> >(sh)` can point at an interpreter.
var fdToSubst = regexp.MustCompile(`[<>]&[3-9]`)

// inertStripped blanks text that is only data (message values, echo/grep arguments, file-only heredoc bodies)
// but keeps every command substitution; pipeBlocks, exfilPatterns and sqlDestructive match this view.
func inertStripped(cmd string) string {
	if strings.Contains(cmd, ">(") || strings.Contains(cmd, "<(") || fdToSubst.MatchString(cmd) {
		return cmd
	}
	segs, seps := shellSegmentsSep(dropInertHeredocs(cmd))
	var b strings.Builder
	for i, seg := range segs {
		// A piped segment feeds the next command, so its text stays: `echo "curl x" | sh` runs curl.
		if inertSegment(seg) && (seps[i] == "" || seps[i][0] != '|' || seps[i] == "||") {
			_, subs := extractSubstitutions(seg)
			seg = ""
			var kept strings.Builder
			for _, sub := range subs {
				kept.WriteString(" $(")
				kept.WriteString(sub)
				kept.WriteString(")")
			}
			seg += kept.String()
		}
		b.WriteString(seg)
		b.WriteString(seps[i])
	}
	return b.String()
}

// inertSegment reports a segment whose words are data: echo/printf, grep/rg patterns, commit/tag/gh text.
func inertSegment(seg string) bool {
	toks := shellTokens(seg)
	if len(toks) == 0 {
		return false
	}
	inert, ok := inertCommands[toks[0]]
	return ok && inert(toks)
}

// inertCommands judge, per command name, whether a segment's words are only data.
var inertCommands = map[string]func(toks []string) bool{
	"echo": inertEcho, "printf": inertEcho, "grep": inertGrep, "rg": inertGrep, "git": inertGit, "gh": inertGh,
}

// inertEcho: a redirect can plant the text in a file that later runs (~/.bashrc).
func inertEcho(toks []string) bool {
	return !slices.ContainsFunc(toks, func(t string) bool { return strings.Contains(t, ">") })
}

func inertGrep(toks []string) bool {
	return !slices.ContainsFunc(toks, func(t string) bool {
		return strings.HasPrefix(t, "--pre") || strings.HasPrefix(t, "--hostname-bin")
	})
}

func inertGit(toks []string) bool {
	return len(toks) > 1 && (toks[1] == "commit" || toks[1] == "tag")
}

func inertGh(toks []string) bool {
	return len(toks) > 3 && (toks[1] == "pr" || toks[1] == "issue") && slices.Contains([]string{"create", "edit", "comment"}, toks[2])
}

// dropInertHeredocs removes bodies of file-only heredocs (`cat <<EOF > f`, `tee f <<EOF`); all other lines stay.
func dropInertHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		delim, quoted, dash, ok := inertHeredocStart(lines[i])
		if !ok {
			continue
		}
		end := heredocEnd(lines, i+1, delim, dash)
		// An unquoted delimiter expands substitutions in the body, so a body holding one is not inert.
		body := strings.Join(lines[i+1:max(end, i+1)], "\n")
		if end < 0 || !quoted && expandsSubstitution(body) {
			continue
		}
		i = end - 1
	}
	return strings.Join(out, "\n")
}

// heredocEnd is the index of the first line from lines[from] that closes the heredoc on delim, or -1.
func heredocEnd(lines []string, from int, delim string, dash bool) int {
	for j := from; j < len(lines); j++ {
		if l := lines[j]; l == delim || dash && strings.TrimLeft(l, "\t") == delim {
			return j
		}
	}
	return -1
}

// expandsSubstitution reports a heredoc body holding a `$(…)` or backtick substitution.
func expandsSubstitution(body string) bool {
	return strings.Contains(body, "$(") || strings.Contains(body, "`")
}

// inertHeredocStart parses a lone `cat|tee … <<DELIM` line that only writes a file; anything fancier is not inert.
func inertHeredocStart(line string) (delim string, quoted, dash, ok bool) {
	if strings.ContainsAny(line, "#|`&(;") || strings.Contains(line, "<<<") || strings.HasSuffix(line, `\`) {
		return
	}
	ms := heredocOp.FindAllStringSubmatch(line, -1)
	if len(ms) != 1 {
		return
	}
	toks := shellTokens(heredocOp.ReplaceAllString(line, " "))
	if len(toks) == 0 || !strings.HasPrefix(strings.TrimLeft(line, " \t"), toks[0]+" ") || !writesFile(toks) {
		return
	}
	m := ms[0]
	delim = m[2] + m[3] + m[4]
	return delim, m[4] == "", m[1] == "-", delim != ""
}

// writesFile reports a `cat` with a `> file` redirect or a `tee` with a file operand.
func writesFile(toks []string) bool {
	switch toks[0] {
	case "tee":
		return slices.ContainsFunc(toks[1:], func(t string) bool {
			return t != "" && t[0] != '-' && !strings.ContainsAny(t, "<>")
		})
	case "cat":
		return catRedirects(toks)
	}
	return false
}

// catRedirects reports a `>`/`>>` token followed by a file, or a `>file` token.
func catRedirects(toks []string) bool {
	for i, t := range toks {
		if (t == ">" || t == ">>") && i+1 < len(toks) || len(t) > 1 && t[0] == '>' {
			return true
		}
	}
	return false
}

// shellTokens splits one segment into unquoted words, dropping a leading `(`/`{` so a subshell reads as its segment.
func shellTokens(seg string) []string {
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

// hardBlockVerdict blocks a hard shape outside inert text, asks on one only in quoted text; `<<` keeps heredoc bodies.
func hardBlockVerdict(in *core.Input, cmd, scan string) (core.Decision, string, string, bool) {
	if strings.Contains(cmd, "<<") {
		scan = cmd
	}
	if pattern, ok := firstHardBlock(scan); ok {
		return core.Block, safetyMessage(in, core.Block, "destructive command", pattern, terminalHint), pattern, true
	}
	if scan == cmd {
		return core.Allow, "", "", false // nothing was stripped, so the raw pass would find nothing new
	}
	if pattern, ok := firstHardBlock(cmd); ok {
		what := "destructive command (matched only inside quoted/inert text)"
		return core.Ask, safetyMessage(in, core.Ask, what, pattern, terminalHint), pattern, true
	}
	return core.Allow, "", "", false
}

// firstHardBlock is the first hard-block pattern any shell segment of text matches.
func firstHardBlock(text string) (string, bool) {
	for _, seg := range shellSegments(text) {
		for _, re := range hardBlocks() {
			if re.MatchString(seg) {
				return re.String(), true
			}
		}
	}
	return "", false
}
