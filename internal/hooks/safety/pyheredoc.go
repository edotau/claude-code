package safety

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

// pyScript is one plain Python stdin heredoc: its body is lines[from:to], the closing delimiter at lines[to].
type pyScript struct{ from, to int }

// dropPyScripts is cmd minus each plain Python stdin script's body: it runs as Python, never as shell words.
func dropPyScripts(cmd string) string {
	return rewritePyScripts(cmd, func(string) string { return "" })
}

// pyUsedText is cmd with each plain Python stdin script reduced to its code and path-shaped string literals.
func pyUsedText(cmd string) string {
	return rewritePyScripts(cmd, func(body string) string {
		if pyDerives().MatchString(pyLex(body, false)) { // a prose string could be cut down to a path
			return body
		}
		return pyLex(body, true)
	})
}

// pyDerives matches code that cuts a string into pieces: split/partition/strip, re, slicing, a literal's method.
var pyDerives = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\.(r?split|r?partition|splitlines|[lr]?strip|removeprefix|removesuffix)\s*\(|` +
		`\bre\s*\.|\[[^\]\n]*:[^\]\n]*\]|""\s*\.\s*\w`)
})

// rewritePyScripts replaces each plain Python stdin script body in cmd with keep(body).
func rewritePyScripts(cmd string, keep func(string) string) string {
	if !strings.Contains(cmd, "<<") || !strings.Contains(cmd, "python") {
		return cmd
	}
	lines := strings.Split(cmd, "\n")
	scripts := pyScripts(lines)
	if len(scripts) == 0 || pyRunsOther(spliceScripts(lines, scripts, func(string) string { return "" })) {
		return cmd
	}
	return spliceScripts(lines, scripts, keep)
}

// spliceScripts is lines rejoined with each script body replaced by keep(body), dropped when that is "".
func spliceScripts(lines []string, scripts []pyScript, keep func(string) string) string {
	var out []string
	last := 0
	for _, s := range scripts {
		out = append(out, lines[last:s.from]...)
		if kept := keep(strings.Join(lines[s.from:s.to], "\n")); kept != "" {
			out = append(out, kept)
		}
		last = s.to
	}
	return strings.Join(append(out, lines[last:]...), "\n")
}

// pyScripts finds top-level heredocs (not inside another body, quote or comment) that feed a plain Python script.
func pyScripts(lines []string) []pyScript {
	var out []pyScript
	var shellText strings.Builder
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		ms := core.HeredocOp().FindAllStringSubmatchIndex(line, -1)
		if len(ms) == 0 {
			shellText.WriteString(line + "\n")
			continue
		}
		at := shell.QuoteAt(shellText.String() + line)
		if at[shellText.Len()+ms[0][0]] != 0 {
			return nil // the operator sits in a quote opened earlier: the lines are not what they seem
		}
		shellText.WriteString(line + "\n")
		end := i
		for _, m := range ms {
			delim := line[max(m[4], m[6], m[8]):max(m[5], m[7], m[9])]
			if end = core.HeredocEnd(lines, end+1, delim, m[3] > m[2]); end < 0 {
				return out
			}
		}
		if len(ms) == 1 && pyStdinLine(line) && pyPlain(lines[i+1:end], ms[0][8] >= 0) {
			out = append(out, pyScript{i + 1, end})
		}
		i = end
	}
	return out
}

// pyInterp matches python, python3 and python3.12, whose `-` or bare stdin is the script.
var pyInterp = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^python[0-9]*(\.[0-9]+)?$`) })

// pyStdinFlags are Python options taking no value that leave stdin as the script.
var pyStdinFlags = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^-[IEsSuBOqbv]+$`) })

// pyStdinLine reports a line whose one heredoc feeds `python [flags] [- args]` on stdin, not piped onward.
func pyStdinLine(line string) bool {
	if strings.Contains(line, "<<<") || strings.Contains(line, ">(") || strings.Contains(line, "<(") {
		return false
	}
	segs, seps := shell.SegmentsSep(line)
	for k, seg := range segs {
		if core.HeredocOp().MatchString(seg) {
			args, ok := pyArgs(seg)
			return ok && seps[k] != "|" && seps[k] != "|&" && pyStdinArgs(args)
		}
	}
	return false
}

// pyArgs is the arguments of a segment that runs python, directly, through a wrapper or `uv run`.
func pyArgs(seg string) ([]string, bool) {
	words, _ := shell.Redirects(seg)
	rest, _ := shell.Unwrap(words, "")
	if len(rest) > 2 && rest[0] == "uv" && rest[1] == "run" {
		rest = rest[2:]
	}
	if len(rest) == 0 || !pyInterp().MatchString(filepath.Base(rest[0])) {
		return nil, false
	}
	return rest[1:], true
}

// pyStdinArgs reports interpreter arguments that keep stdin the script: value-free flags, then nothing or `-`.
func pyStdinArgs(args []string) bool {
	for _, a := range args {
		if a == "-" {
			return true
		}
		if !pyStdinFlags().MatchString(a) {
			return false
		}
	}
	return true
}

// pyImportsOK are stdlib modules a plain edit script imports; none runs a command or loads code.
var pyImportsOK = []string{"re", "json", "pathlib", "os", "shutil", "io", "sys", "textwrap", "collections",
	"itertools", "functools", "glob", "fnmatch", "difflib", "string", "datetime", "time", "math", "hashlib", "csv",
	"tomllib", "typing", "dataclasses", "argparse", "unicodedata", "pprint", "statistics", "enum", "copy",
	"operator", "bisect", "heapq", "tempfile", "filecmp", "uuid", "random", "decimal", "fractions", "html"}

// pyHandOff matches code that runs a command or builds code: exec/eval, os.system/popen/exec*/spawn*, reflection.
var pyHandOff = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\b(exec|eval|compile|execfile|getattr|setattr|delattr|globals|locals|vars|breakpoint|` +
		`help|import_module|modules|system|popen[0-9]?|spawn\w*|posix_spawn\w*|exec[lv]\w*|fork\w*|startfile)\b|` +
		`__\w+__`)
})

// pyImport matches each import statement's module list, wherever it sits (`if x: import y`).
var pyImport = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\bfrom[ \t]+([\w.]*)[ \t]+import\b|\bimport[ \t]+([^;\n]+)`)
})

// pyPlain reports a body that hands nothing to a shell or to dynamic code, so its strings are only data.
func pyPlain(body []string, bare bool) bool {
	text := strings.Join(body, "\n")
	if bare && substitutes(text) { // a bare delimiter expands $( ) first
		return false
	}
	code := pyLex(text, false)
	return !slices.ContainsFunc(pyHandOff().FindAllString(code, -1), func(m string) bool {
		return m != "__name__" && m != "__main__" && m != "__file__"
	}) && pyImportsPlain(code)
}

// pyImportsPlain reports every module code imports is in pyImportsOK.
func pyImportsPlain(code string) bool {
	for _, m := range pyImport().FindAllStringSubmatch(code, -1) {
		for mod := range strings.SplitSeq(m[1]+m[2], ",") {
			root, _, _ := strings.Cut(strings.Fields(mod + " .")[0], ".")
			if !slices.Contains(pyImportsOK, root) {
				return false
			}
		}
	}
	return true
}

// pyLex is Python minus comments and string literals but f-strings (keepPaths: also one-word ones, maybe a path).
func pyLex(src string, keepPaths bool) string {
	var b strings.Builder
	for i := 0; i < len(src); i++ {
		switch c := src[i]; c {
		case '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case '\'', '"':
			end := pyStringEnd(src, i)
			lit := src[i:end]
			if pyFString(src, i) || keepPaths && len(strings.Fields(strings.Trim(lit, `'"`))) <= 1 {
				b.WriteString(lit)
			} else {
				b.WriteString(`""`)
			}
			i = end - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// pyStringEnd is the index just past the string literal opening at src[i], single or triple quoted.
func pyStringEnd(src string, i int) int {
	q := src[i : i+1]
	if strings.HasPrefix(src[i:], q+q+q) {
		q = q + q + q
	}
	for j := i + len(q); j < len(src); j++ {
		switch {
		case src[j] == '\\':
			j++
		case strings.HasPrefix(src[j:], q):
			return j + len(q)
		case len(q) == 1 && src[j] == '\n':
			return j
		}
	}
	return len(src)
}

// pyFString reports a literal at src[i] whose prefix (f, rf, fr, any case) makes it an f-string.
func pyFString(src string, i int) bool {
	j := i
	for j > 0 && strings.IndexByte("rRbBuUfF", src[j-1]) >= 0 {
		j--
	}
	return strings.ContainsAny(src[j:i], "fF") && (j == 0 || !isIdentByte(src[j-1]))
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// pyRunsOther reports command text outside its Python scripts that runs a file or shell a script may have written.
func pyRunsOther(rest string) bool {
	for _, seg := range shell.Segments(rest) {
		if _, py := pyArgs(seg); (!py || !core.HeredocOp().MatchString(seg)) && core.RunsWrittenFile().MatchString(seg) {
			return true
		}
	}
	return false
}
