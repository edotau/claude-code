package hooks

import (
	"regexp"
	"slices"
	"strings"
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
				kept.WriteString(" $(" + sub + ")")
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
	switch toks[0] {
	case "echo", "printf": // a redirect can plant the text in a file that later runs (~/.bashrc)
		return !slices.ContainsFunc(toks, func(t string) bool { return strings.Contains(t, ">") })
	case "grep", "rg":
		return !slices.ContainsFunc(toks, func(t string) bool {
			return strings.HasPrefix(t, "--pre") || strings.HasPrefix(t, "--hostname-bin")
		})
	case "git":
		return len(toks) > 1 && (toks[1] == "commit" || toks[1] == "tag")
	case "gh":
		return len(toks) > 3 && (toks[1] == "pr" || toks[1] == "issue") && slices.Contains([]string{"create", "edit", "comment"}, toks[2])
	}
	return false
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
		end := -1
		for j := i + 1; j < len(lines) && end < 0; j++ {
			if l := lines[j]; l == delim || dash && strings.TrimLeft(l, "\t") == delim {
				end = j
			}
		}
		// An unquoted delimiter expands substitutions in the body, so a body holding one is not inert.
		body := strings.Join(lines[i+1:max(end, i+1)], "\n")
		if end < 0 || !quoted && (strings.Contains(body, "$(") || strings.Contains(body, "`")) {
			continue
		}
		i = end - 1
	}
	return strings.Join(out, "\n")
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
		for _, t := range toks[1:] {
			if t != "" && t[0] != '-' && !strings.ContainsAny(t, "<>") {
				return true
			}
		}
	case "cat":
		for i, t := range toks {
			if (t == ">" || t == ">>") && i+1 < len(toks) || len(t) > 1 && t[0] == '>' {
				return true
			}
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
			if quote == '"' && c == '\\' && i+1 < len(seg) && dqEscapable(seg[i+1]) {
				i++
				b.WriteByte(seg[i])
				continue
			}
			if c == quote {
				quote = 0
			} else {
				b.WriteByte(c)
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
			started = true
		case c == '\\' && i+1 < len(seg):
			i++
			b.WriteByte(seg[i])
			started = true
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			flush()
		case c == ')' || c == '}':
			flush() // unquoted close of a subshell/group: a word boundary, never part of the word
		default:
			b.WriteByte(c)
			started = true
		}
	}
	flush()
	if len(out) > 0 {
		out[0] = strings.TrimLeft(out[0], "({")
		if out[0] == "" {
			out = out[1:]
		}
	}
	return out
}
