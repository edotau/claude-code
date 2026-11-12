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

// WrittenPaths is the best-effort set of files a shell command writes (sed/perl -i, redirects, tee, cp/mv), made
// absolute against cwd; interpreter bodies (`python -c`, `node -e`, a script file) are opaque and yield nothing.
func WrittenPaths(cmd, cwd string) []string {
	if !MayWrite(cmd) {
		return nil
	}
	return appendTargets(nil, DropDataHeredocs(cmd), cwd, 0, writeTargets)
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
