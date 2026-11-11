package safety

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/shell"
)

// dropRemoteHeredocs removes quoted-delimiter heredoc bodies fed to ssh: they run remotely, unexpanded locally.
func dropRemoteHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		m := heredocOp().FindStringSubmatch(lines[i])
		if m == nil || m[4] != "" || !sshHoldsHeredoc(lines[i]) { // m[4]: bare delimiter, expanded locally
			continue
		}
		if end := heredocEnd(lines, i+1, m[2]+m[3]+m[4], m[1] == "-"); end >= 0 {
			i = end - 1
		}
	}
	return strings.Join(out, "\n")
}

// sshHoldsHeredoc reports an ssh segment carrying the heredoc operator whose output is not piped on.
func sshHoldsHeredoc(line string) bool {
	segs, seps := shell.SegmentsSep(line)
	for i := 0; i < len(segs); i++ {
		seg, sep := segs[i], seps[i]
		for i+1 < len(segs) && sep == "&" && redirectJoin(seg, segs[i+1]) { // `2>&1` / `&>f` are not separators
			i++
			seg, sep = seg+"&"+segs[i], seps[i]
		}
		if heredocOp().MatchString(seg) {
			toks := shell.Tokens(seg)
			return len(toks) > 0 && toks[0] == "ssh" && sep != "|" && sep != "|&"
		}
	}
	return false
}

// redirectJoin reports that the `&` between left and right belongs to a redirection (`>&`, `&>`).
func redirectJoin(left, right string) bool {
	return strings.HasSuffix(strings.TrimSpace(left), ">") || strings.HasPrefix(right, ">")
}

// heredocOp matches one `<<[-]DELIM` operator; groups: dash, single-quoted, double-quoted, bare delimiter.
var heredocOp = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`<<(-?)\s*(?:'([^']*)'|"([^"]*)"|([A-Za-z_][A-Za-z0-9_]*))`)
})

// fdToSubst matches a redirect to fd 3+, which `exec 3> >(sh)` can point at an interpreter.
var fdToSubst = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[<>]&[3-9]`) })

// inertStripped blanks text that is only data (message values, echo/grep arguments, file-only heredoc bodies)
// but keeps every command substitution; pipeBlocks, exfilPatterns and sqlDestructive match this view.
func inertStripped(cmd string) string {
	if strings.Contains(cmd, ">(") || strings.Contains(cmd, "<(") || fdToSubst().MatchString(cmd) {
		return cmd
	}
	segs, seps := shell.SegmentsSep(dropInertHeredocs(cmd))
	var b strings.Builder
	for i, seg := range segs {
		// A piped segment feeds the next command, so its text stays: `echo "curl x" | sh` runs curl.
		if inertSegment(seg) && (seps[i] == "" || seps[i][0] != '|' || seps[i] == "||") {
			_, subs := shell.ExtractSubstitutions(seg)
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
	toks := shell.Tokens(seg)
	if len(toks) == 0 {
		return false
	}
	inert, ok := inertCommands[toks[0]]
	return ok && inert(toks)
}

// inertCommands judge, per command name, whether a segment's words are only data.
var inertCommands = map[string]func(toks []string) bool{
	"echo": inertEcho, "printf": inertEcho, "grep": inertGrep, "rg": inertGrep, "git": inertGit, "gh": inertGh,
	"claude-code": inertScm,
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

// inertScm: the --title of `claude-code scm pr create|comment` is data; a --body-file path or a redirect is not.
func inertScm(toks []string) bool {
	if len(toks) < 4 || toks[1] != "scm" || !inertEcho(toks) ||
		slices.ContainsFunc(toks, func(t string) bool { return strings.HasPrefix(t, "--body-file") }) {
		return false
	}
	return (toks[2] == "pr" && (toks[3] == "create" || toks[3] == "comment")) || toks[2] == "comment"
}

// dropInertHeredocs removes file-only heredoc bodies unless the command also runs a file.
func dropInertHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	if runsWrittenFile().MatchString(lines[0]) || heredocTail(lines) {
		return cmd
	}
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
	ms := heredocOp().FindAllStringSubmatch(line, -1)
	if len(ms) != 1 {
		return
	}
	toks := shell.Tokens(heredocOp().ReplaceAllString(line, " "))
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

// runsWrittenFile matches a command that executes a file (a shell, source, ., exec, an interpreter or chmod +x).
var runsWrittenFile = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)(^|[;&|\s])(bash|sh|zsh|source|exec|python3?|node|perl|ruby)\s|(^|[;&|])\s*\.\s|chmod\s+\+?[0-7]*x`)
})

// dropDataHeredocs removes quoted-delimiter `cat > f`/`tee f` heredoc bodies so hard rule 4 never asks on text written
// to a file; a command that also runs a file keeps every body (`cat <<'EOF' > f.sh … EOF && bash f.sh`).
func dropDataHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	if runsWrittenFile().MatchString(lines[0]) || heredocTail(lines) {
		return cmd
	}
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		delim, ok := dataHeredocStart(lines[i])
		if !ok {
			continue
		}
		if end := heredocEnd(lines, i+1, delim, false); end > i {
			i = end - 1
		}
	}
	return strings.Join(out, "\n")
}

// heredocTail reports any line after a heredoc's close (or any non-start line outside one) that runs a file.
func heredocTail(lines []string) bool {
	for i := 0; i < len(lines); i++ {
		delim, _, dash, ok := inertHeredocStart(lines[i])
		if !ok {
			delim, ok = dataHeredocStart(lines[i])
		}
		if ok {
			if end := heredocEnd(lines, i+1, delim, dash); end > i {
				i = end
				continue
			}
		}
		if i > 0 && runsWrittenFile().MatchString(lines[i]) {
			return true
		}
	}
	return false
}

// dataHeredocStart finds one quoted `<<'DELIM'` on a segment that is `cat > f` or `tee f`; other segments on the line
// (`mkdir -p d && cat > d/f <<'EOF'`) are checked as usual.
func dataHeredocStart(line string) (string, bool) {
	ms := heredocOp().FindAllStringSubmatch(line, -1)
	if len(ms) != 1 || ms[0][1] == "-" || ms[0][4] != "" {
		return "", false
	}
	for _, seg := range shell.Segments(line) {
		if !heredocOp().MatchString(seg) {
			continue
		}
		toks := shell.Tokens(heredocOp().ReplaceAllString(seg, " "))
		return ms[0][2] + ms[0][3], len(toks) > 0 && writesFile(toks)
	}
	return "", false
}
