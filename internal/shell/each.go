package shell

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxNest bounds substitution/`bash -c`/`eval` recursion so a pathological command cannot loop.
const maxNest = 4

// shellKeywords lead a segment without being its command (`if cd x`, `! git`, `then git`).
var shellKeywords = []string{"if", "then", "else", "elif", "do", "while", "until", "!", "{", "("}

// Uninspectable is the verb EachGit reports, with the cwd as target, for nesting deeper than maxNest: the callee cannot
// see what runs there, so a guard must fail closed on it.
const Uninspectable = "<uninspectable>"

// gitFn receives one git invocation: its target tree, verb and the verb's words; true stops the walk.
type gitFn func(target, verb string, args []string) bool

// EachGit calls fn with each git invocation in cmd and the tree it targets, until fn returns true (reported back).
// It follows substitutions, nested `bash -c`/`eval`, wrappers, `env -C`/`sudo -D`, and `cd`/`pushd` between segments.
// Nesting past maxNest reports one Uninspectable call instead of being skipped.
func EachGit(cmd, cwd string, fn gitFn) bool {
	return eachGit(cmd, cwd, fn, 0)
}

func eachGit(cmd, cwd string, fn gitFn, depth int) bool {
	if depth > maxNest {
		return fn(cwd, Uninspectable, nil)
	}
	masked, subs := extractSubstitutions(cmd, subMask)
	for _, sub := range subs { // a substitution runs nested: its cd stays inside it
		if eachGit(sub, cwd, fn, depth+1) {
			return true
		}
	}
	segs, seps := SegmentsSep(masked)
	w := &segWalk{cwd: cwd, segs: segs, seps: seps, fn: fn, depth: depth}
	for i, seg := range segs {
		w.enter(seg)
		if w.run(i) {
			return true
		}
		w.leave(seg)
	}
	return false
}

// segWalk is one eachGit pass over a command's segments.
type segWalk struct {
	cwdTracker
	segs, seps []string
	fn         gitFn
	depth      int
}

// run visits segment i: a cd moves the tracker, git words and nested text are reported to fn.
func (w *segWalk) run(i int) bool {
	toks := commandWords(w.segs[i])
	if len(toks) == 0 {
		return false
	}
	prev := ""
	if i > 0 {
		prev = w.seps[i-1]
	}
	at, isCd := w.step(toks, prev, w.seps[i])
	if isCd {
		return false
	}
	rest, dir := Unwrap(toks, at)
	if segmentGit(rest, dir, w.fn, w.depth) {
		return true
	}
	if w.pipesIntoShell(i) {
		return pipedIntoShell(rest, dir, w.fn, w.depth)
	}
	return false
}

// pipesIntoShell reports that segment i feeds a bare shell that reads its commands from stdin.
func (w *segWalk) pipesIntoShell(i int) bool {
	return (w.seps[i] == "|" || w.seps[i] == "|&") && i+1 < len(w.segs) && bareShell(commandWords(w.segs[i+1]))
}

// dirExists is the seam for "does this cd target exist"; a cd to a missing directory fails and leaves cwd alone.
var dirExists = statDir

func statDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// cwdTracker follows the shell cwd across subshells, pipes, `||` after a cd, missing targets and pushd/popd.
type cwdTracker struct {
	cwd, before string // before: the dir the previous segment's cd left, for a following `||`
	moved       bool
	subshell    []string // cwd to restore at each open `(`'s close
	dirs        []string // pushd stack
}

func (c *cwdTracker) enter(seg string) {
	opens, _ := parenCounts(seg)
	for range opens {
		c.subshell = append(c.subshell, c.cwd)
	}
}

func (c *cwdTracker) leave(seg string) {
	_, closes := parenCounts(seg)
	for range closes {
		if n := len(c.subshell); n > 0 {
			c.cwd, c.subshell = c.subshell[n-1], c.subshell[:n-1]
			c.moved = false
		}
	}
}

// step returns the dir a segment runs in (the old one after a cd that `||` makes conditional) and applies a builtin
// cd/pushd/popd; a pipe side or backgrounded segment is a subshell, so its cd is not applied.
func (c *cwdTracker) step(toks []string, prev, sep string) (at string, isCd bool) {
	at = c.cwd
	if prev == "||" && c.moved {
		at = c.before
	}
	c.moved = false
	next, ok := c.cdTarget(builtinPrefixDropped(toks), !subshellSide(prev, sep))
	if ok {
		c.apply(next, prev, sep)
	}
	return at, ok
}

// cdTarget is the cwd a cd/pushd/popd leaves; a missing directory fails the cd, so cwd stays. live: pushd records cwd.
func (c *cwdTracker) cdTarget(cmd []string, live bool) (string, bool) {
	if len(cmd) > 0 && cmd[0] == "popd" {
		return popDir(&c.dirs, c.cwd), true
	}
	next, ok := CdTarget(cmd, c.cwd)
	if !ok {
		return c.cwd, false
	}
	if missingDir(next, c.cwd) {
		next = c.cwd
	}
	if live && next != c.cwd && cmd[0] == "pushd" {
		c.dirs = append(c.dirs, c.cwd)
	}
	return next, true
}

// missingDir reports a cd target that is absolute, differs from cwd and does not exist.
func missingDir(next, cwd string) bool {
	return filepath.IsAbs(next) && next != cwd && !dirExists(next)
}

// apply moves the cwd unless this segment is a pipe side or backgrounded.
func (c *cwdTracker) apply(next, prev, sep string) {
	if next != c.cwd && !subshellSide(prev, sep) {
		c.before, c.cwd, c.moved = c.cwd, next, true
	}
}

func subshellSide(prev, sep string) bool {
	return prev == "|" || prev == "|&" || sep == "|" || sep == "|&" || sep == "&"
}

func popDir(dirs *[]string, cwd string) string {
	n := len(*dirs)
	if n == 0 {
		return cwd
	}
	d := (*dirs)[n-1]
	*dirs = (*dirs)[:n-1]
	return d
}

// parenCounts counts the unquoted `(` and `)` in a segment (substitutions are already masked out).
func parenCounts(seg string) (opens, closes int) {
	var quote byte
	for i := 0; i < len(seg); i++ {
		if quote != 0 {
			i, quote = quotedStep(seg, i, quote)
			continue
		}
		switch seg[i] {
		case '\\':
			i++
		case '\'', '"':
			quote = seg[i]
		case '(':
			opens++
		case ')':
			closes++
		}
	}
	return opens, closes
}

// builtinPrefixDropped drops what still runs the next word in the current shell (NAME=val, `command`, `builtin`, `time`),
// unlike env/sudo/nohup/nice/timeout/exec, which start another process: a `cd` behind those cannot move this shell.
func builtinPrefixDropped(toks []string) []string {
	for {
		toks = skipEnvAssignments(toks)
		if len(toks) == 0 || toks[0] != "command" && toks[0] != "builtin" && toks[0] != "time" {
			return toks
		}
		toks, _ = skipWrapperOpts(toks[0], toks[1:], "")
	}
}

// commandWords is the segment's words without leading shell keywords; nil for an ssh command (it runs remotely).
func commandWords(seg string) []string {
	toks := Tokens(seg)
	for len(toks) > 0 && slices.Contains(shellKeywords, toks[0]) {
		toks = toks[1:]
	}
	if len(toks) > 0 && toks[0] == "ssh" {
		return nil
	}
	return toks
}

// inertCommands print or search their arguments: a git-looking string there is text, not a command.
var inertCommands = map[string]bool{"echo": true, "printf": true, "grep": true, "egrep": true, "fgrep": true, "rg": true,
	"ag": true, "cat": true, "tee": true, "head": true, "tail": true, "sed": true, "awk": true, "jq": true, "gh": true}

// segmentGit visits the nested command and each git word of one segment whose wrappers are already dropped.
func segmentGit(toks []string, dir string, fn gitFn, depth int) bool {
	if len(toks) == 0 {
		return false
	}
	if nested, ok := nestedOf(toks); ok { // its words are the nested command's, visited once there
		return eachGit(nested, dir, fn, depth+1)
	}
	if text, ok := hereString(toks); ok {
		return eachGit(text, dir, fn, depth+1)
	}
	s := &segScan{dir: dir, fn: fn, depth: depth, inert: inertCommands[filepath.Base(toks[0])]}
	for i := range toks { // any word: xargs/watch/find -exec need no modelling
		if s.word(toks, i) {
			return true
		}
	}
	return false
}

// segScan is the state of one segmentGit word scan.
type segScan struct {
	dir             string
	fn              gitFn
	depth           int
	inert, afterGit bool
	done            string // a -c string already visited
}

func (s *segScan) nest(text string) bool { return eachGit(text, s.dir, s.fn, s.depth+1) }

// word visits toks[i]: a git word, a shell wrapper's -c string, or a multi-word string that reads as a command line.
func (s *segScan) word(toks []string, i int) bool {
	t := toks[i]
	if (t == "git" || strings.HasSuffix(t, "/git")) && filepath.Base(t) == "git" {
		s.afterGit = true
		return s.fn(resolvedGit(toks[i+1:], s.dir))
	}
	switch {
	case i == 0:
	case shellWrapperNames[filepath.Base(t)]: // `find -exec sh -c 'STR'`, `setsid sh -c 'STR'`
		if nested, ok := nestedOf(toks[i:]); ok {
			s.done = nested
			return s.nest(nested)
		}
	case cStringCommands[filepath.Base(toks[0])] && isCFlag(toks[i-1]): // `su -c 'STR'`, `script -qc 'STR'`
		s.done = t
		return s.nest(t)
	case s.textArg(t): // `watch 'git x'`
		return s.nest(t)
	}
	return false
}

// textArg reports a command-line-looking argument that is neither inert, after git's own words, nor already visited.
func (s *segScan) textArg(t string) bool {
	return !s.afterGit && !s.inert && t != s.done && commandText(t)
}

// cStringCommands run the string after their -c flag through a shell.
var cStringCommands = map[string]bool{"su": true, "script": true, "flock": true}

func isCFlag(t string) bool {
	return len(t) > 1 && t[0] == '-' && t[1] != '-' && strings.ContainsRune(t[1:], 'c')
}

// commandText reports a multi-word token that reads as a command line: its first word is git or a shell wrapper.
func commandText(t string) bool {
	f := strings.Fields(t)
	if len(f) < 2 {
		return false
	}
	name := filepath.Base(f[0])
	return name == "git" || shellWrapperNames[name]
}

// hereString returns the string a bare shell reads from `<<< STR` (also attached `<<<STR`).
func hereString(toks []string) (string, bool) {
	if !bareShell(toks) {
		return "", false
	}
	for i, t := range toks {
		switch {
		case t == "<<<" && i+1 < len(toks):
			return toks[i+1], true
		case strings.HasPrefix(t, "<<<") && len(t) > 3:
			return t[3:], true
		}
	}
	return "", false
}

// bareShell reports a shell with no -c string and no script file: it reads its commands from stdin.
func bareShell(toks []string) bool {
	rest, _ := Unwrap(toks, "")
	if len(rest) == 0 || !shellWrapperNames[filepath.Base(rest[0])] {
		return false
	}
	if _, ok := nestedOf(rest); ok {
		return false
	}
	for _, t := range rest[1:] {
		if !strings.HasPrefix(t, "-") && !strings.ContainsAny(t, "<>") {
			return false
		}
	}
	return true
}

// pipedIntoShell inspects the output of the command piped to a bare shell: an echo/printf literal is nested text,
// anything else is not visible, so a guard must fail closed.
func pipedIntoShell(toks []string, dir string, fn gitFn, depth int) bool {
	if len(toks) > 0 && (toks[0] == "echo" || toks[0] == "printf") {
		var words []string
		for _, t := range toks[1:] {
			if !strings.HasPrefix(t, "-") {
				words = append(words, t)
			}
		}
		if formatUnapplied(toks[0], words) {
			return fn(dir, Uninspectable, nil)
		}
		return eachGit(strings.Join(words, " "), dir, fn, depth+1)
	}
	return fn(dir, Uninspectable, nil)
}

// formatUnapplied reports echo/printf output the joined text would misread: a backslash escape, or a printf % verb.
func formatUnapplied(verb string, words []string) bool {
	if slices.ContainsFunc(words, func(w string) bool { return strings.Contains(w, `\`) }) {
		return true
	}
	return verb == "printf" && len(words) > 0 && strings.Contains(words[0], "%")
}
