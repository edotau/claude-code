package core

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/shell"
)

// DropHeredocBodies drops each body whose closing line bodyEnd returns (-1 keeps it); guarded keeps all if a file runs.
func DropHeredocBodies(cmd string, guarded bool, bodyEnd func(lines []string, i int) int) string {
	lines := strings.Split(cmd, "\n")
	if guarded && (RunsWrittenFile().MatchString(lines[0]) || heredocTail(lines)) {
		return cmd
	}
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		if end := bodyEnd(lines, i); end > i {
			i = end - 1
		}
	}
	return strings.Join(out, "\n")
}

// HeredocOp matches one `<<[-]DELIM` operator; groups: dash, single-quoted, double-quoted, bare delimiter.
var HeredocOp = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`<<(-?)\s*(?:'([^']*)'|"([^"]*)"|([A-Za-z_][A-Za-z0-9_]*))`)
})

// HeredocEnd is the index of the first line from lines[from] that closes the heredoc on delim, or -1.
func HeredocEnd(lines []string, from int, delim string, dash bool) int {
	for j := from; j < len(lines); j++ {
		if l := lines[j]; l == delim || dash && strings.TrimLeft(l, "\t") == delim {
			return j
		}
	}
	return -1
}

// InertHeredocStart parses a lone `cat|tee … <<DELIM` line that only writes a file; anything fancier is not inert.
func InertHeredocStart(line string) (delim string, quoted, dash, ok bool) {
	if strings.ContainsAny(line, "#|`&(;") || strings.Contains(line, "<<<") || strings.HasSuffix(line, `\`) {
		return
	}
	ms := HeredocOp().FindAllStringSubmatch(line, -1)
	if len(ms) != 1 {
		return
	}
	toks := shell.Tokens(HeredocOp().ReplaceAllString(line, " "))
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

// RunsWrittenFile matches a command that executes a file (shell, source, exec, interpreter, chmod +x).
var RunsWrittenFile = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)(^|[;&|(/\x60\s])((ba|z|da|k)?sh|fish|pwsh|source|exec|python[0-9.]*|node|perl|ruby|deno|bun|php)\s|(^|[;&|])\s*\.[\s/]|chmod\s+[-+=,ugoarwXst0-7]*x`)
})

// DropDataHeredocs removes quoted-delimiter `cat > f`/`tee f` heredoc bodies unless the command also runs a file.
func DropDataHeredocs(cmd string) string {
	return DropHeredocBodies(cmd, true, func(lines []string, i int) int {
		delim, ok := dataHeredocStart(lines[i])
		if !ok {
			return -1
		}
		return HeredocEnd(lines, i+1, delim, false)
	})
}

// shellReader names a command whose stdin heredoc body runs as shell commands.
var shellReader = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^((ba|z|da|k|mk)?sh|fish|source|\.|eval|exec|xargs)$`)
})

// DropStdinHeredocs drops bodies no shell reads; a shell or `>(` on the line, or `$(` in a bare body, keeps it.
func DropStdinHeredocs(cmd string) string {
	return DropHeredocBodies(cmd, false, func(lines []string, i int) int {
		line := lines[i]
		m := HeredocOp().FindAllStringSubmatch(line, -1)
		if len(m) != 1 || strings.Contains(line, "<<<") || strings.Contains(line, ">(") ||
			strings.Contains(line, "<(") || feedsShell(line) {
			return -1
		}
		end := HeredocEnd(lines, i+1, m[0][2]+m[0][3]+m[0][4], m[0][1] == "-")
		if end < 0 || m[0][4] != "" && strings.ContainsAny(strings.Join(lines[i+1:end], "\n"), "$`") {
			return -1
		}
		return end
	})
}

// feedsShell reports a line where any segment's command, or a file it writes then runs, is a shell.
func feedsShell(line string) bool {
	if writesFileHeredoc(line) {
		return true // cat > f / tee f bodies are DropDataHeredocs's to judge, with its run-the-file guard
	}
	for _, seg := range shell.Segments(HeredocOp().ReplaceAllString(line, " ")) {
		rest, _ := shell.Unwrap(shell.Tokens(seg), "")
		if len(rest) > 0 && shellReader().MatchString(filepath.Base(rest[0])) {
			return true
		}
	}
	return false
}

// writesFileHeredoc reports a heredoc segment whose command is a cat/tee that writes a file.
func writesFileHeredoc(line string) bool {
	for _, seg := range shell.Segments(line) {
		if HeredocOp().MatchString(seg) {
			toks := shell.Tokens(HeredocOp().ReplaceAllString(seg, " "))
			return len(toks) > 0 && writesFile(toks)
		}
	}
	return false
}

// heredocTail reports any line after a heredoc's close (or any non-start line outside one) that runs a file.
func heredocTail(lines []string) bool {
	for i := 0; i < len(lines); i++ {
		delim, _, dash, ok := InertHeredocStart(lines[i])
		if !ok {
			delim, ok = dataHeredocStart(lines[i])
		}
		if ok {
			if end := HeredocEnd(lines, i+1, delim, dash); end > i {
				if i > 0 && RunsWrittenFile().MatchString(lines[i]) {
					return true // the start line itself runs a file
				}
				i = end
				continue
			}
		}
		if i > 0 && RunsWrittenFile().MatchString(lines[i]) {
			return true
		}
	}
	return false
}

// dataHeredocStart finds one quoted `<<'DELIM'` on a `cat > f` or `tee f` segment; other segments are checked as usual.
func dataHeredocStart(line string) (string, bool) {
	ms := HeredocOp().FindAllStringSubmatch(line, -1)
	// A <<< here-string has no body; `>(` / `<(` feed a process that may run the body.
	if len(ms) != 1 || ms[0][1] == "-" || ms[0][4] != "" || strings.Contains(line, "<<<") ||
		strings.Contains(line, ">(") || strings.Contains(line, "<(") {
		return "", false
	}
	return ms[0][2] + ms[0][3], writesFileHeredoc(line)
}
