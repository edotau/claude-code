package safety

import (
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	rest := runnerUnwrap(words)
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

// pyRunners run a program in a managed environment (`uv run`, `poetry run`, …), with their value-taking options.
var pyRunners = map[string]map[string]bool{
	"uv": {"--with": true, "--with-editable": true, "--with-requirements": true, "--python": true, "-p": true,
		"--project": true, "--directory": true, "--package": true, "--extra": true, "--group": true, "--env-file": true,
		"--index": true, "--default-index": true, "--index-url": true, "--extra-index-url": true, "-w": true},
	"poetry": {"-C": true, "--directory": true, "-P": true, "--project": true},
	"pipx":   {"--spec": true, "--python": true, "--pip-args": true, "--index-url": true},
	"conda":  {"-n": true, "--name": true, "-p": true, "--prefix": true, "--cwd": true},
}

// runnerUnwrap is toks past wrappers and `[python -m] uv|poetry|pipx|conda run [opts]`, from the program run.
func runnerUnwrap(toks []string) []string {
	rest, _ := shell.Unwrap(toks, "")
	for len(rest) > 2 {
		if pyInterp().MatchString(filepath.Base(rest[0])) && rest[1] == "-m" && pyRunners[rest[2]] != nil {
			rest = rest[2:]
		}
		opts := pyRunners[filepath.Base(rest[0])]
		if opts == nil || len(rest) < 2 || rest[1] != "run" {
			return rest
		}
		rest = rest[2:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
			opt := rest[0]
			rest = rest[1:]
			if opt == "--" {
				break
			}
			if opts[opt] && len(rest) > 0 {
				rest = rest[1:]
			}
		}
	}
	return rest
}

// withPyLaunches is view with the literal commands each Python script launches, in a subshell chained before it.
func withPyLaunches(view string) string {
	if !strings.Contains(view, "python") {
		return view
	}
	outer, bodies, ok := liftBodies(view)
	if !ok {
		return view
	}
	segs, seps := shell.SegmentsSep(outer)
	var b strings.Builder
	added, lineOps := false, 0
	for i, seg := range segs {
		if i > 0 && seps[i-1] == "\n" {
			lineOps = 0
		}
		if code, ok := pyCode(segs, i, bodies, lineOps == 0); ok {
			if cmds := pyLaunches(code); len(cmds) > 0 {
				b.WriteString("( " + strings.Join(cmds, " && ") + " ) && ") // && keeps a dynamic cd's chain
				added = true
			}
		}
		lineOps += len(core.HeredocOp().FindAllString(seg, -1))
		b.WriteString(seg + seps[i])
	}
	if !added {
		return view
	}
	return spliceBodies(b.String(), bodies)
}

// pyCode is the Python segs[i] runs: its -c code, or the stdin heredoc body (the line's only operator) after it.
func pyCode(segs []string, i int, bodies []string, firstOp bool) (string, bool) {
	args, ok := pyArgs(segs[i])
	if !ok {
		return "", false
	}
	if ops := len(core.HeredocOp().FindAllString(segs[i], -1)); ops > 0 {
		if ops > 1 || !firstOp || !pyStdinArgs(args) {
			return "", false
		}
		for _, s := range segs[i+1:] {
			if n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), bodySlot)); err == nil &&
				strings.HasPrefix(strings.TrimSpace(s), bodySlot) && n < len(bodies) {
				return bodies[n], true
			}
			if core.HeredocOp().MatchString(s) {
				return "", false
			}
		}
		return "", false
	}
	return pyDashC(args)
}

// pyCFlag matches python's -c, alone or ending a cluster of value-free flags.
var pyCFlag = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^-[IEsSuBOqbv]*c$`) })

// pyDashC is the code after python's -c, before any script, module or `-`.
func pyDashC(args []string) (string, bool) {
	for i := 0; i < len(args)-1; i++ {
		switch a := args[i]; {
		case pyCFlag().MatchString(a):
			return args[i+1], true
		case a == "-X" || a == "-W":
			i++
		case !strings.HasPrefix(a, "-") || a == "-" || a == "-m":
			return "", false
		}
	}
	return "", false
}

// pyLaunchCall matches a call that runs a process: shell strings, argv lists, os.spawn*/exec*, asyncio, pty.
var pyLaunchCall = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\b(system|popen|getoutput|getstatusoutput|run|call|check_call|check_output|Popen|` +
		`spawn|create_subprocess_shell|create_subprocess_exec|(?:exec|spawn)[lv]p?e?|posix_spawnp?)\s*\(`)
})

// pyLaunches are the commands code launches whose words are all literals, as shell text.
func pyLaunches(code string) []string {
	code = pyBlank(code, false)
	mask := pyBlank(code, true)
	var out []string
	for _, m := range pyLaunchCall().FindAllStringSubmatchIndex(mask, -1) {
		args, end, ok := pyItems(code, m[1]-1)
		if !ok || end < 0 {
			continue
		}
		if cmd, ok := pyLaunch(mask[m[2]:m[3]], args); ok && shellSafe(cmd) {
			out = append(out, pyInDir(code, mask, m[0], args, cmd))
		}
	}
	return out
}

// pyBlank is src with comments, and string literals when strs, turned to spaces; offsets and newlines stay.
func pyBlank(src string, strs bool) string {
	b := []byte(src)
	for i := 0; i < len(src); i++ {
		var end int
		switch src[i] {
		case '#':
			if end = strings.IndexByte(src[i:], '\n') + i; end < i {
				end = len(src)
			}
		case '\'', '"':
			if end = pyStringEnd(src, i); !strs {
				i = end - 1
				continue
			}
		default:
			continue
		}
		for j := i; j < end; j++ {
			if b[j] != '\n' {
				b[j] = ' '
			}
		}
		i = end - 1
	}
	return string(b)
}

// pyItems splits the bracketed text opening at src[open] into trimmed top-level items; end is its closer's index.
func pyItems(src string, open int) (items []string, end int, ok bool) {
	depth, from := 0, open+1
	for i := open; i < len(src); i++ {
		switch c := src[i]; {
		case c == '\'' || c == '"':
			i = pyStringEnd(src, i) - 1
		case strings.IndexByte("([{", c) >= 0:
			depth++
		case strings.IndexByte(")]}", c) >= 0:
			if depth--; depth == 0 {
				if last := strings.TrimSpace(src[from:i]); last != "" {
					items = append(items, last)
				}
				return items, i, true
			}
		case c == ',' && depth == 1:
			items = append(items, strings.TrimSpace(src[from:i]))
			from = i + 1
		}
	}
	return nil, -1, false
}

// pyKwarg matches a keyword argument `name=value`.
var pyKwarg = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?s)^([A-Za-z_]\w*)\s*=([^=].*)$`) })

// pyLaunch is the shell text a launch call named name runs with args, when every word of it is a literal.
func pyLaunch(name string, args []string) (string, bool) {
	var pos []string
	kw := map[string]string{}
	for _, a := range args {
		if strings.HasPrefix(a, "*") {
			return "", false // *argv: the words are not in the source
		}
		if m := pyKwarg().FindStringSubmatch(a); m != nil {
			kw[m[1]] = strings.TrimSpace(m[2])
		} else {
			pos = append(pos, a)
		}
	}
	switch {
	case name == "create_subprocess_exec":
		return pyArgv(pos)
	case strings.HasPrefix(name, "posix_spawn"):
		return pyPathArgv(pos, true)
	case strings.HasPrefix(name, "spawn") && name != "spawn":
		if len(pos) == 0 {
			return "", false
		}
		pos = pos[1:] // the P_WAIT/P_NOWAIT mode
		fallthrough
	case strings.HasPrefix(name, "exec"):
		v := strings.Contains(name, "v")
		if !v && strings.HasSuffix(name, "e") && len(pos) > 0 {
			pos = pos[:len(pos)-1] // execle/spawnle: the env mapping
		}
		return pyPathArgv(pos, v)
	}
	first, ok := kw["args"]
	if !ok && len(pos) > 0 {
		first, ok = pos[0], true
	}
	if !ok {
		return "", false
	}
	if s, ok := pyStr(first); ok {
		return s, true // a string is shell text, or a program name that runs the same way
	}
	argv, ok := pyStrList(first)
	if !ok || len(argv) == 0 {
		return "", false
	}
	if kw["shell"] == "True" {
		return argv[0], true
	}
	return shellJoin(argv), true
}

// pyPathArgv is exec/spawn's program path plus argv past argv[0], from an argv list (v) or the remaining args.
func pyPathArgv(pos []string, v bool) (string, bool) {
	if len(pos) < 2 {
		return "", false
	}
	path, ok := pyStr(pos[0])
	argv, okArgs := pyStrList(pos[1])
	if !v {
		argv, okArgs = pyStrs(pos[1:])
	}
	if !ok || !okArgs || len(argv) == 0 {
		return "", false
	}
	return shellJoin(append([]string{path}, argv[1:]...)), true
}

// pyArgv is the shell text of literal positional words.
func pyArgv(pos []string) (string, bool) {
	argv, ok := pyStrs(pos)
	return shellJoin(argv), ok && len(argv) > 0
}

// pyStrList is a list or tuple display whose items are all string literals.
func pyStrList(t string) ([]string, bool) {
	if t == "" || t[0] != '[' && t[0] != '(' {
		return nil, false
	}
	items, end, ok := pyItems(t, 0)
	if !ok || end != len(t)-1 {
		return nil, false
	}
	return pyStrs(items)
}

func pyStrs(items []string) ([]string, bool) {
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := pyStr(it)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// pyEscapes decodes the escapes a command literal uses; others stay as written.
var pyEscapes = strings.NewReplacer(`\\`, `\`, `\'`, `'`, `\"`, `"`, `\n`, "\n", `\t`, "\t")

// pyStr is the value of t when t is exactly one string literal; an f-string is built at run time, so never.
func pyStr(t string) (string, bool) {
	i := 0
	for i < len(t) && i < 2 && strings.IndexByte("rRbBuU", t[i]) >= 0 {
		i++
	}
	if i >= len(t) || t[i] != '\'' && t[i] != '"' || pyStringEnd(t, i) != len(t) {
		return "", false
	}
	q := 1
	if len(t)-i >= 6 && strings.HasPrefix(t[i:], strings.Repeat(t[i:i+1], 3)) {
		q = 3
	}
	if len(t)-i < 2*q {
		return "", false
	}
	body := t[i+q : len(t)-q]
	if strings.ContainsAny(t[:i], "rR") {
		return body, true
	}
	return pyEscapes.Replace(body), true
}

// shellPlain matches a word the shell reads unchanged without quotes.
var shellPlain = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`) })

func shellJoin(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = a
		if !shellPlain().MatchString(a) {
			words[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}

// shellSafe reports launched text that cannot run past its subshell: closed quotes and parens, no heredoc or NUL.
func shellSafe(cmd string) bool {
	at := shell.QuoteAt(cmd + " x")
	return cmd != "" && at[len(at)-1] == 0 && !strings.ContainsAny(cmd, "\x00") && !strings.Contains(cmd, "<<") &&
		!strings.HasSuffix(cmd, `\`) && strings.Count(cmd, "(") == strings.Count(cmd, ")")
}

// pyInDir is cmd run in the directory its call sets: a literal `cwd=`/earlier os.chdir, or a fresh tempfile dir.
func pyInDir(code, mask string, at int, args []string, cmd string) string {
	val, set := "", false
	for _, a := range args {
		if m := pyKwarg().FindStringSubmatch(a); m != nil && m[1] == "cwd" {
			val, set = strings.TrimSpace(m[2]), true
		}
	}
	if !set {
		val = lastPyChdir(code, mask[:at])
	}
	if dir, ok := pyStr(val); ok && dir != "" {
		return "( cd " + shellJoin([]string{dir}) + " && " + cmd + " )"
	}
	if pyTempDir(code, val) {
		return `( cd "$(mktemp -d)" && ` + cmd + " )"
	}
	return cmd
}

// pyChdir matches an os.chdir call.
var pyChdir = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\bos\.chdir\s*\(`) })

// lastPyChdir is the argument of the last os.chdir call in before, or "".
func lastPyChdir(code, before string) string {
	ms := pyChdir().FindAllStringIndex(before, -1)
	if len(ms) == 0 {
		return ""
	}
	items, _, ok := pyItems(code, ms[len(ms)-1][1]-1)
	if !ok || len(items) != 1 {
		return ""
	}
	return items[0]
}

var pyIdent = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z_]\w*$`) })

// pyTempDir reports val as a tempfile call, or a name code only ever binds to a tempfile call.
func pyTempDir(code, val string) bool {
	if strings.HasPrefix(val, "tempfile.") {
		return true
	}
	if !pyIdent().MatchString(val) {
		return false
	}
	name := regexp.QuoteMeta(val)
	via := regexp.MustCompile(`\btempfile\.\w+\([^)]*\)\s+as\s+` + name + `\b`).MatchString(code)
	n, temp := 0, 0
	for _, m := range regexp.MustCompile(`(?m)(?:^|[;\s])`+name+`\s*=([^=].*)$`).FindAllStringSubmatch(code, -1) {
		n++
		if strings.HasPrefix(strings.TrimSpace(m[1]), "tempfile.") {
			temp++
		}
	}
	return via && n == 0 || n > 0 && n == temp
}
