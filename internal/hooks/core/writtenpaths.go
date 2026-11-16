package core

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// nestedDepth caps `bash -c "…"` recursion.
const nestedDepth = 2

// writeHints are the substrings a command needs to write a file; none present means nothing to parse.
var writeHints = []string{">", "sed", "perl", "tee", "cp", "mv", "install"}

// redirectTok matches a redirect word: optional fd or `&`, `>`/`>>`, then the target when glued on.
var redirectTok = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(?:\d*|&)>>?\|?(.*)$`) })

// WrittenPaths is the best-effort set of files a shell command writes, made absolute; interpreter bodies yield none.
func WrittenPaths(cmd, cwd string) []string {
	if !MayWrite(cmd) {
		return nil
	}
	return appendTargets(nil, DropDataHeredocs(cmd), cwd, 0, writeTargets)
}

// WrittenPathsPrecise is WrittenPaths minus quoted/commented `>`, stdin heredocs and relative paths after a dynamic cd.
func WrittenPathsPrecise(cmd, cwd string) []string {
	all := WrittenPaths(cmd, cwd)
	if len(all) == 0 {
		return nil
	}
	precise := appendPrecise(nil, dropComments(DropStdinHeredocs(DropDataHeredocs(cmd))), cwd, 0)
	// Intersect: never more than WrittenPaths, so a gate moving here can only allow more.
	return slices.DeleteFunc(all, func(p string) bool { return !slices.Contains(precise, p) })
}

// appendPrecise is appendTargets that reads only unquoted redirects and drops relative targets after a dynamic cd.
func appendPrecise(out []string, cmd, cwd string, depth int) []string {
	for _, seg := range shell.Segments(cmd) {
		toks := shell.Tokens(seg)
		if nested, ok := shell.NestedCommand(toks); ok && depth < nestedDepth {
			out = appendPrecise(out, nested, cwd, depth+1)
			continue
		}
		if dir, ok := shell.CdTarget(toks, cwd); ok {
			cwd = dir
			if dynamicCd(toks) {
				cwd = "" // unknown: only absolute targets can still be judged
			}
			continue
		}
		rest, _ := shell.Unwrap(toks, cwd)
		targets := commandTargets(rest)
		if shell.FileRedirect(seg) {
			targets = append(redirectTargets(toks), targets...)
		}
		for _, t := range targets {
			if p := resolveTarget(t, cwd); filepath.IsAbs(p) && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// dynamicCd reports a cd/pushd whose target is `-` or still holds an expansion, so the shell's cwd is unknown.
func dynamicCd(toks []string) bool {
	args := toks[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		opt := args[0]
		if args = args[1:]; opt == "--" {
			break
		}
	}
	if len(args) == 0 {
		return false
	}
	p := args[0]
	for _, home := range []string{"$HOME", "${HOME}"} {
		if rest, ok := strings.CutPrefix(p, home); ok && (rest == "" || rest[0] == '/') {
			p = rest
		}
	}
	return p == "-" || strings.ContainsAny(p, "$`*?\x00")
}

// dropComments cuts each unquoted `#` that starts a word through the end of its line.
func dropComments(cmd string) string {
	for from := 0; ; {
		at, i := shell.QuoteAt(cmd), -1
		for k := from; k < len(cmd) && i < 0; k++ {
			if cmd[k] == '#' && at[k] == 0 && (k == 0 || at[k-1] == 0 && strings.IndexByte(" \t\n;&|()", cmd[k-1]) >= 0) {
				i = k
			}
		}
		if i < 0 {
			return cmd
		}
		end := strings.IndexByte(cmd[i:], '\n')
		if end < 0 {
			return cmd[:i]
		}
		cmd, from = cmd[:i]+cmd[i+end:], i+1
	}
}

// RemovedPaths is the best-effort set of files a shell command deletes or moves away (rm, unlink, truncate, mv).
func RemovedPaths(cmd, cwd string) []string {
	if !strings.Contains(cmd, "rm") && !strings.Contains(cmd, "unlink") && !strings.Contains(cmd, "truncate") &&
		!strings.Contains(cmd, "mv") {
		return nil
	}
	return appendTargets(nil, DropDataHeredocs(cmd), cwd, 0, func(_, rest []string) []string { return removalTargets(rest) })
}

// writeTargets are a segment's redirect targets plus the files its command word writes.
func writeTargets(toks, rest []string) []string {
	return append(redirectTargets(toks), commandTargets(rest)...)
}

// removalTargets are the operands rm, unlink and truncate remove, and the sources mv moves away.
func removalTargets(toks []string) []string {
	if len(toks) < 2 {
		return nil
	}
	switch filepath.Base(toks[0]) {
	case "rm", "unlink", "truncate":
		return operands(toks[1:])
	case "mv":
		ops := operands(toks[1:])
		if len(ops) < 2 {
			return nil
		}
		return ops[:len(ops)-1]
	}
	return nil
}

// MayWrite reports whether cmd contains anything that could write a file; a cheap gate before WrittenPaths.
func MayWrite(cmd string) bool {
	for _, h := range writeHints {
		if strings.Contains(cmd, h) {
			return true
		}
	}
	return false
}

// appendTargets adds pick's targets for each segment of cmd to out, unique, descending into `bash -c` bodies.
func appendTargets(out []string, cmd, cwd string, depth int, pick func(toks, rest []string) []string) []string {
	for _, seg := range shell.Segments(cmd) {
		toks := shell.Tokens(seg)
		if nested, ok := shell.NestedCommand(toks); ok && depth < nestedDepth {
			out = appendTargets(out, nested, cwd, depth+1, pick)
			continue
		}
		if dir, ok := shell.CdTarget(toks, cwd); ok {
			cwd = dir
			continue
		}
		rest, _ := shell.Unwrap(toks, cwd)
		for _, t := range pick(toks, rest) {
			if p := resolveTarget(t, cwd); p != "" && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// resolveTarget expands ~ and a leading $HOME and anchors a relative target; "" for /dev/*, fd dups and substitutions.
func resolveTarget(t, cwd string) string {
	if t == "" || t[0] == '&' || t[0] == '(' || strings.HasPrefix(t, "/dev/") || strings.Contains(t, "$(") {
		return ""
	}
	for _, home := range []string{"$HOME", "${HOME}"} {
		if rest, ok := strings.CutPrefix(t, home); ok && (rest == "" || rest[0] == '/') {
			t = "~" + rest
		}
	}
	return AnchorPath(paths.ExpandHome(t), cwd)
}

// redirectTargets are the files a segment's `>`, `>>`, `2>`, `&>` words name.
func redirectTargets(toks []string) []string {
	var out []string
	for i, t := range toks {
		m := redirectTok().FindStringSubmatch(t)
		if m == nil {
			continue
		}
		if m[1] != "" {
			out = append(out, m[1])
		} else if i+1 < len(toks) {
			out = append(out, toks[i+1])
		}
	}
	return out
}

// commandTargets are the files the command word itself writes: sed -i, perl -i, tee, cp, mv, install.
func commandTargets(toks []string) []string {
	if len(toks) < 2 {
		return nil
	}
	args := toks[1:]
	switch filepath.Base(toks[0]) {
	case "sed":
		return inPlaceFiles(args, "ef")
	case "perl":
		return inPlaceFiles(args, "eE")
	case "tee":
		return operands(args)
	case "cp", "mv", "install":
		return destinations(args)
	}
	return nil
}

// operands are the non-flag words of args.
func operands(args []string) []string {
	var out []string
	for _, a := range args {
		if a != "" && a[0] != '-' {
			out = append(out, a)
		}
	}
	return out
}

// inPlaceFiles are the file operands of sed/perl when an in-place flag is present, else nil.
func inPlaceFiles(args []string, scriptFlags string) []string {
	inPlace, script := false, false
	var files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--"):
			inPlace = inPlace || strings.HasPrefix(a, "--in-place")
		case a == "" || a == "-":
		case a[0] == '-':
			ip, takesScript := clusterFlags(a, scriptFlags)
			inPlace = inPlace || ip
			if takesScript {
				script, i = true, i+1
			}
		case !script:
			script = true // the first operand is the script (sed) or program file (perl)
		default:
			files = append(files, a)
		}
	}
	if !inPlace {
		return nil
	}
	return files
}

// clusterFlags reads a short-flag cluster: whether it holds -i (the rest is its suffix) and a script flag.
func clusterFlags(a, scriptFlags string) (inPlace, takesScript bool) {
	for _, c := range a[1:] {
		if c == 'i' {
			return true, takesScript
		}
		takesScript = takesScript || strings.ContainsRune(scriptFlags, c)
	}
	return false, takesScript
}

// destinations is the `-t DIR` value, else the last operand of cp/mv/install (under it per source if a directory).
func destinations(args []string) []string {
	var ops []string
	for i := range len(args) {
		a := args[i]
		switch {
		case a == "-t" && i+1 < len(args):
			return []string{args[i+1]}
		case strings.HasPrefix(a, "--target-directory="):
			return []string{strings.TrimPrefix(a, "--target-directory=")}
		case a != "" && a[0] != '-':
			ops = append(ops, a)
		}
	}
	if len(ops) < 2 {
		return nil
	}
	return intoDir(ops[len(ops)-1], ops[:len(ops)-1])
}

// intoDir is dst, or each source's name under dst when dst is an existing directory.
func intoDir(dst string, srcs []string) []string {
	if st, err := os.Stat(paths.ExpandHome(dst)); err != nil || !st.IsDir() {
		return []string{dst}
	}
	out := make([]string, len(srcs))
	for i, s := range srcs {
		out[i] = filepath.Join(dst, filepath.Base(s))
	}
	return out
}

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
