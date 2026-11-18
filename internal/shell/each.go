package shell

import (
	"cmp"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// maxNest bounds substitution/`bash -c`/`eval` recursion so a pathological command cannot loop.
const maxNest = 4

// Uninspectable is EachGit's verb (cwd as target) for nesting past maxNest, unseen code a guard must fail closed on.
const Uninspectable = "<uninspectable>"

// WorktreeCommit is EachGit's verb for a `claude-code worktree commit` call; its args are the verb's own words.
const WorktreeCommit = "worktree commit"

const tooDeepArg = "<too-deep>"

// TooDeep reports an Uninspectable call's args as nesting past maxNest, not a verb the shell expands.
func TooDeep(args []string) bool { return len(args) == 1 && args[0] == tooDeepArg }

// gitFn receives one git invocation: its target tree, verb and the verb's words; true stops the walk.
type gitFn func(target, verb string, args []string) bool

// EachGit calls fn per git invocation in cmd and its target tree, through nesting, wrappers and cd, until fn is true.
func EachGit(cmd, cwd string, fn gitFn) bool {
	return eachGit(cmd, cwd, fn, 0, false)
}

// eachGit is EachGit at a nesting depth; runs marks text whose output is itself run, so no command in it is inert.
func eachGit(cmd, cwd string, fn gitFn, depth int, runs bool) bool {
	if depth > maxNest {
		return fn(cwd, Uninspectable, []string{tooDeepArg})
	}
	masked, subs := extractSubstitutions(cmd, subMask)
	if len(subs) > 0 {
		executed := executedSubs(masked, len(subs))
		for k, sub := range subs { // a substitution runs nested: its cd stays inside it
			if eachGit(sub, cwd, fn, depth+1, runs || executed[k]) {
				return true
			}
		}
	}
	segs, seps := SegmentsSep(masked)
	var scripts []Script
	if strings.Contains(masked, ">") || strings.Contains(masked, "tee") {
		scripts = runScripts(segs, seps, cwd)
	}
	walk, writing := fn, false
	var seen map[[2]string]bool
	if len(scripts) > 0 {
		seen = map[[2]string]bool{}
		walk = func(target, verb string, args []string) bool {
			seen[[2]string{target, verb}] = seen[[2]string{target, verb}] || writing
			return fn(target, verb, args)
		}
	}
	w := &segWalk{cwd: cwd, segs: segs, seps: seps, words: make([][]string, len(segs)),
		wordsSet: make([]bool, len(segs)), fn: walk, depth: depth, runs: runs}
	for i, seg := range segs {
		w.enter(seg)
		writing = len(scripts) > 0 && w.writesThenRuns(i) // only the writing segment's text repeats in its script
		if w.run(i) {
			return true
		}
		w.leave(seg)
	}
	return nestScripts(scripts, seen, fn, depth, runs)
}

// nestScripts walks each written-then-run script's text, `printf 'git x\n' > f && sh f`, skipping calls already seen.
func nestScripts(scripts []Script, seen map[[2]string]bool, fn gitFn, depth int, runs bool) bool {
	fresh := func(target, verb string, args []string) bool {
		return !seen[[2]string{target, verb}] && fn(target, verb, args)
	}
	for _, s := range scripts {
		if eachGit(s.Text, s.Dir, fresh, depth+1, runs) {
			return true
		}
	}
	return false
}

// segWalk is one eachGit pass over a command's segments.
type segWalk struct {
	cwdTracker
	segs, seps []string
	words      [][]string
	wordsSet   []bool
	fn         gitFn
	depth      int
	runs       bool
	inertBody  []bool // segments that are the body of a heredoc fed to an inert command
}

func (w *segWalk) cmdWords(i int) []string {
	if !w.wordsSet[i] {
		w.words[i] = commandWords(w.segs[i])
		w.wordsSet[i] = true
	}
	return w.words[i]
}

// run visits segment i: a cd moves the tracker, git words and nested text are reported to fn.
func (w *segWalk) run(i int) bool {
	toks := w.cmdWords(i)
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
	fn := w.fn
	if w.heredocText(i) {
		fn = func(target, verb string, args []string) bool {
			return verb != WorktreeCommit && w.fn(target, verb, args)
		}
	}
	if segmentGit(rest, dir, fn, w.depth, w.runs || w.pipesIntoRunner(i) || w.writesThenRuns(i)) {
		return true
	}
	if w.pipesIntoShell(i) {
		return pipedIntoShell(rest, dir, w.fn, w.depth, w.runs)
	}
	return false
}

// heredocText reports segment i as the body of a heredoc that only an inert command (cat, tee) reads.
func (w *segWalk) heredocText(i int) bool {
	if w.inertBody == nil {
		w.inertBody = make([]bool, len(w.segs))
		for j := range w.segs {
			delim, ok := heredocDelim(w.cmdWords(j))
			rest, _ := Unwrap(w.cmdWords(j), "")
			if rest = dropRtk(rest); !ok || len(rest) == 0 || !inertCommand(rest) {
				continue
			}
			for k := j + 1; k < len(w.segs) && strings.TrimSpace(w.segs[k]) != delim; k++ {
				w.inertBody[k] = true
			}
		}
	}
	return w.inertBody[i]
}

// heredocDelim is the delimiter word of a `<<DELIM` or `<<- DELIM` in toks.
func heredocDelim(toks []string) (string, bool) {
	for i, t := range toks {
		switch {
		case t == "<<" || t == "<<-":
			if i+1 < len(toks) {
				return toks[i+1], true
			}
		case strings.HasPrefix(t, "<<") && !strings.HasPrefix(t, "<<<"):
			return strings.TrimLeft(t[2:], "-"), true
		}
	}
	return "", false
}

// pipesIntoRunner reports that segment i feeds a runner (xargs, eval, a shell with a script) its words to run.
func (w *segWalk) pipesIntoRunner(i int) bool {
	if w.seps[i] != "|" && w.seps[i] != "|&" || i+1 >= len(w.segs) {
		return false
	}
	next := w.cmdWords(i + 1)
	rest, _ := Unwrap(next, "")
	return len(rest) > 0 && runner(rest[0]) && !bareShell(next) // a bare shell's input is pipedIntoShell's
}

// writesThenRuns reports segment i writing a file while a later segment runs a script or a command by path.
func (w *segWalk) writesThenRuns(i int) bool {
	if !FileRedirect(w.segs[i]) {
		return false
	}
	for j := i + 1; j < len(w.segs); j++ {
		rest, _ := Unwrap(w.cmdWords(j), "")
		if len(rest) > 1 && runner(rest[0]) || len(rest) > 0 && strings.Contains(rest[0], "/") {
			return true
		}
	}
	return false
}

// runArgs run an argument as a command line or script; shellWrapperNames do too.
var runArgs = map[string]bool{"eval": true, "source": true, ".": true, "xargs": true, "watch": true, "su": true,
	"script": true, "flock": true}

func runner(name string) bool {
	return runArgs[filepath.Base(name)] || shellWrapperNames[filepath.Base(name)]
}

// executedSubs marks each substitution whose output runs: a command word, a runner's argument, or a var it expands.
func executedSubs(masked string, n int) []bool {
	out := make([]bool, n)
	var assigned []int
	varRun, k := false, 0
	for _, seg := range Segments(masked) {
		toks := Tokens(seg)
		kw := 0
		for kw < len(toks) && isShellKeyword(toks[kw]) {
			kw++
		}
		rest, _ := Unwrap(toks[kw:], "")
		at := max(kw, len(toks)-len(rest))
		for j, t := range toks {
			code := j == at || j > at && len(rest) > 0 && runner(rest[0])
			varRun = varRun || code && strings.Contains(t, "$")
			for range strings.Count(t, subMask) {
				switch {
				case k >= n:
				case code:
					out[k] = true
				case j < at && isEnvAssign(t):
					assigned = append(assigned, k)
				}
				k++
			}
		}
	}
	for _, a := range assigned {
		out[a] = out[a] || varRun
	}
	return out
}

// pipesIntoShell reports that segment i feeds a bare shell that reads its commands from stdin.
func (w *segWalk) pipesIntoShell(i int) bool {
	return (w.seps[i] == "|" || w.seps[i] == "|&") && i+1 < len(w.segs) && bareShell(w.cmdWords(i+1)) &&
		!w.casePattern(i)
}

// casePatternHead matches the segment `case WORD in PATTERN` that opens a case pattern list.
var casePatternHead = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\bcase\s.*\sin\s+\S+$`) })

// casePattern reports the `|` after segment i as a case pattern separator (`in sh|bash) …`), not a pipe.
func (w *segWalk) casePattern(i int) bool {
	head, last := i, i+1
	for head > 0 && w.seps[head-1] == "|" {
		head--
	}
	for last < len(w.segs)-1 && w.seps[last] == "|" {
		last++
	}
	bare := func(s string) bool { return s != "" && !strings.ContainsAny(strings.TrimSpace(s), " \t") }
	opens := head == 0 || w.seps[head-1] == ";;" || w.seps[head-1] == "\n"
	if h := strings.TrimSpace(w.segs[head]); !casePatternHead().MatchString(h) && (!opens || !bare(h)) {
		return false
	}
	for _, s := range w.segs[head+1 : last] {
		if !bare(s) {
			return false
		}
	}
	before, _, ok := strings.Cut(w.segs[last], ")")
	return ok && bare(before)
}

// dirExists is the seam for "does this cd target exist"; a cd to a missing directory fails and leaves cwd alone.
var dirExists = statDir

func statDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// unknownDir is the cwd reported after a cd whose target is dynamic: absolute, never a real dir or under one.
var unknownDir = filepath.Join(string(filepath.Separator), "<unknown cwd>")

// UnknownDir reports the target EachGit gives git run after a dynamic cd in the same `&&` chain.
func UnknownDir(dir string) bool { return dir == unknownDir }

// cwdTracker follows the shell cwd across subshells, pipes, `||` after a cd, missing targets and pushd/popd.
type cwdTracker struct {
	cwdState
	before   string // the dir the previous segment's cd left, for a following `||`
	start    string // the dir the command started in, set by the first step
	moved    bool
	subshell []cwdState // the state to restore at each open `(`'s close
	dirs     []string   // pushd stack
}

// cwdState is what a subshell's close restores.
type cwdState struct {
	cwd    string
	exact  string // after a dynamic or missing cd target, the dir for the rest of its `&&` chain
	lost   bool   // a dynamic cd took effect: cwd is only the starting dir's guess
	oldpwd string // where `cd -` goes; "" until a cd runs
}

// here is the dir the next segment runs in.
func (c *cwdTracker) here() string { return cmp.Or(c.exact, c.cwd) }

func (c *cwdTracker) enter(seg string) {
	opens, _ := parenCounts(seg)
	for range opens {
		c.subshell = append(c.subshell, c.cwdState)
	}
}

func (c *cwdTracker) leave(seg string) {
	_, closes := parenCounts(seg)
	for range closes {
		if n := len(c.subshell); n > 0 {
			c.cwdState, c.subshell = c.subshell[n-1], c.subshell[:n-1]
			c.moved = false
		}
	}
}

// EachSegmentDir calls fn per non-cd segment with its dir, tracked as EachGit does; lost: a dynamic cd moved it.
func EachSegmentDir(cmd, cwd string, fn func(seg string, toks []string, dir string, lost bool)) {
	segs, seps := SegmentsSep(cmd)
	eachSegment(segs, seps, cwd, func(i int, toks []string, dir string, lost bool) { fn(segs[i], toks, dir, lost) })
}

// eachSegment is EachSegmentDir over split segments, passing each segment's index.
func eachSegment(segs, seps []string, cwd string, fn func(i int, toks []string, dir string, lost bool)) {
	c := &cwdTracker{cwd: cwd}
	for i, seg := range segs {
		c.enter(seg)
		toks, prev := Tokens(seg), ""
		if i > 0 {
			prev = seps[i-1]
		}
		words := toks
		for len(words) > 0 && isShellKeyword(words[0]) {
			words = words[1:]
		}
		if at, isCd := c.step(words, prev, seps[i]); !isCd && len(toks) > 0 {
			lost := c.lost || at == unknownDir
			if at == unknownDir {
				at = c.cwd
			}
			fn(i, toks, at, lost)
		}
		c.leave(seg)
	}
}

// step returns a segment's dir and applies a builtin cd/pushd/popd, except on a pipe side or backgrounded segment.
func (c *cwdTracker) step(toks []string, prev, sep string) (at string, isCd bool) {
	if prev != "&&" && prev != "|" && prev != "|&" { // a failed cd no longer stops what follows
		c.exact = ""
	}
	at = c.here()
	if prev == "||" && c.moved {
		at = c.before
	}
	c.moved = false
	if c.start == "" {
		c.start = c.cwd
	}
	cmd := c.dashResolved(builtinPrefixDropped(toks))
	next, exact, ok := c.cdTarget(cmd, !subshellSide(prev, sep))
	if ok && !subshellSide(prev, sep) {
		abs, dynamic, _ := cdResolve(cmd, "")
		c.lost = exact == unknownDir || c.lost && (dynamic || !filepath.IsAbs(abs))
		c.oldpwd = at
	}
	if ok {
		c.apply(next, exact, prev, sep)
	}
	return at, ok
}

// dashResolved turns `cd -` into a cd to the previous dir; an unknown one is the starting dir, the conservative side.
func (c *cwdTracker) dashResolved(cmd []string) []string {
	if len(cmd) == 0 || cmd[0] != "cd" {
		return cmd
	}
	if args := skipCdOptions(cmd[1:]); len(args) == 0 || args[0] != "-" {
		return cmd
	}
	prev := c.oldpwd
	if prev == "" || prev == unknownDir {
		prev = c.start
	}
	return []string{"cd", prev}
}

// cdTarget is the cwd a cd/pushd/popd leaves (a missing dir: cwd stays) and exact, where it lands when it succeeds.
func (c *cwdTracker) cdTarget(cmd []string, live bool) (next, exact string, ok bool) {
	if len(cmd) > 0 && cmd[0] == "popd" {
		d := popDir(&c.dirs, c.cwd)
		return d, d, true
	}
	next, ok = CdTarget(cmd, c.cwd)
	if !ok {
		return c.cwd, c.here(), false
	}
	exact, dynamic, _ := cdResolve(cmd, c.here())
	if dynamic {
		exact = unknownDir
	}
	if missingDir(next, c.cwd) {
		next = c.cwd
	}
	if live && next != c.cwd && cmd[0] == "pushd" {
		c.dirs = append(c.dirs, c.cwd)
	}
	return next, exact, true
}

// missingDir reports a cd target that is absolute, differs from cwd and does not exist.
func missingDir(next, cwd string) bool {
	return filepath.IsAbs(next) && next != cwd && !dirExists(next)
}

// apply moves the cwd unless this segment is a pipe side or backgrounded.
func (c *cwdTracker) apply(next, exact, prev, sep string) {
	if subshellSide(prev, sep) {
		return
	}
	if next != c.cwd {
		c.before, c.cwd, c.moved = c.cwd, next, true
	}
	c.exact = ""
	if exact != next {
		c.exact = exact
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
	if !strings.ContainsAny(seg, "()") {
		return 0, 0
	}
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

// builtinPrefixDropped drops NAME=val, `command`, `builtin`, `time`: prefixes whose cd still moves this shell.
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
	for len(toks) > 0 && isShellKeyword(toks[0]) {
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

// inertHarnessVerbs are the claude-code verbs whose arguments are search terms, facts or PR text, never run.
var inertHarnessVerbs = map[string]bool{"memory recall": true, "memory search": true, "memory remember": true,
	"network websearch": true, "scm pr": true, "scm comment": true}

// inertCommand reports a command whose arguments are text: an inertCommands name or an inertHarnessVerbs verb.
func inertCommand(toks []string) bool {
	if filepath.Base(toks[0]) == "claude-code" {
		return len(toks) > 2 && inertHarnessVerbs[toks[1]+" "+toks[2]]
	}
	return inertCommands[filepath.Base(toks[0])]
}

// segmentGit visits the nested command and each git word of one segment whose wrappers are already dropped.
func segmentGit(toks []string, dir string, fn gitFn, depth int, runs bool) bool {
	if toks = dropRtk(toks); len(toks) == 0 {
		return false
	}
	if nested, ok := nestedOf(toks); ok { // its words are the nested command's, visited once there
		return eachGit(nested, dir, fn, depth+1, runs)
	}
	if text, ok := hereString(toks); ok {
		return eachGit(text, dir, fn, depth+1, runs)
	}
	s := &segScan{dir: dir, fn: fn, depth: depth, runs: runs, inert: !runs && inertCommand(toks)}
	for i := range toks { // any word: xargs/watch/find -exec need no modelling
		if s.word(toks, i) {
			return true
		}
	}
	return false
}

// segScan is the state of one segmentGit word scan.
type segScan struct {
	dir                   string
	fn                    gitFn
	depth                 int
	runs, inert, afterGit bool
	done                  string // a -c string already visited
}

func (s *segScan) nest(text string) bool { return eachGit(text, s.dir, s.fn, s.depth+1, s.runs) }

// word visits toks[i]: a git word, a shell wrapper's -c string, or a multi-word string that reads as a command line.
func (s *segScan) word(toks []string, i int) bool {
	t := toks[i]
	if !s.inert && (t == "git" || strings.HasSuffix(t, "/git")) && filepath.Base(t) == "git" {
		s.afterGit = true
		return s.fn(resolvedGit(toks[i+1:], s.dir))
	}
	if !s.inert && i+2 < len(toks) && toks[i+1] == "worktree" && toks[i+2] == "commit" && harnessCommandWord(toks, i) {
		return s.fn(s.dir, WorktreeCommit, toks[i+3:])
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
	case s.textArg(t, toks[0]): // `watch 'git x'`
		return s.nest(t)
	}
	return false
}

// textArg reports a command-line-looking argument that is neither inert, after git's own words, nor already visited.
func (s *segScan) textArg(t, head string) bool {
	if s.afterGit || s.inert || t == s.done || !commandText(t) {
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(t), "claude-code ") || runner(head) // claude-code text runs only under a runner
}

// CommandStarts lists the words from each command word of a segment, through keywords, wrappers, xargs and find -exec.
func CommandStarts(toks []string) [][]string {
	var out [][]string
	for range maxNest * 2 {
		for len(toks) > 0 && isShellKeyword(toks[0]) {
			toks = toks[1:]
		}
		if len(toks) == 0 {
			break
		}
		out = append(out, toks)
		if toks = afterPrefix(skipEnvAssignments(toks)); toks == nil {
			break
		}
	}
	return out
}

// execFlags are find's flags that run the words after them as a command.
var execFlags = []string{"-exec", "-execdir", "-ok", "-okdir"}

// xargsValueFlags take the next word as their value.
var xargsValueFlags = []string{"-n", "-I", "-L", "-P", "-s", "-d", "-E", "-a"}

// afterPrefix is the words after a leading wrapper (rtk, env, sudo, xargs, find -exec) and its options, else nil.
func afterPrefix(toks []string) []string {
	if len(toks) == 0 {
		return nil
	}
	switch name := filepath.Base(toks[0]); {
	case name == "rtk":
		return dropRtk(toks)
	case wrapperNames[name]:
		rest, _ := skipWrapperOpts(name, toks[1:], "")
		if wrapperOperands[name] && len(rest) > 0 {
			rest = rest[1:]
		}
		if name == "env" && len(rest) > 0 && rest[0] == "-" {
			rest = rest[1:]
		}
		return rest
	case name == "xargs":
		rest := toks[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "-") && rest[0] != "--" {
			n := 1
			if slices.Contains(xargsValueFlags, rest[0]) {
				n = 2
			}
			rest = rest[min(n, len(rest)):]
		}
		return rest
	case name == "find":
		for i, t := range toks {
			if slices.Contains(execFlags, t) {
				return toks[i+1:]
			}
		}
	}
	return nil
}

// dropRtk drops a leading `rtk` and its flags, so `rtk grep` reads as `grep`.
func dropRtk(toks []string) []string {
	if len(toks) == 0 || filepath.Base(toks[0]) != "rtk" {
		return toks
	}
	toks = toks[1:]
	for len(toks) > 0 && strings.HasPrefix(toks[0], "-") {
		toks = toks[1:]
	}
	return toks
}

// harnessCommandWord reports toks[i] as claude-code in command position: first, after `go run`, or under a runner.
func harnessCommandWord(toks []string, i int) bool {
	if filepath.Base(toks[i]) != "claude-code" {
		return false
	}
	switch name := filepath.Base(toks[0]); {
	case i == 0:
		return true
	case name == "go":
		return toks[1] == "run" && optionsOnly(toks[2:i])
	case name == "find":
		return slices.Contains(execFlags, toks[i-1])
	case name == "xargs" || name == "watch":
		return optionsOnly(toks[1:i])
	}
	return false
}

// optionsOnly reports words that are all flags or a flag's value (`-n 1`), never a command of their own.
func optionsOnly(words []string) bool {
	for i, w := range words {
		if !strings.HasPrefix(w, "-") && (i == 0 || !strings.HasPrefix(words[i-1], "-")) {
			return false
		}
	}
	return true
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
	return name == "git" || shellWrapperNames[name] || name == "claude-code" && f[1] == "worktree"
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

// pipedIntoShell nests an echo/printf literal piped to a bare shell; any other source is Uninspectable.
func pipedIntoShell(toks []string, dir string, fn gitFn, depth int, runs bool) bool {
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
		return eachGit(strings.Join(words, " "), dir, fn, depth+1, runs)
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

// Script is text a command line writes to a file with echo/printf (a redirect or tee) and then runs from that file.
type Script struct {
	Text, Dir string
	Lost      bool // a dynamic cd moved the run: Dir is only the starting dir's guess
}

// RunScripts lists each file cmd writes from printed text that a later segment runs (`sh f`, `. f`, `./f`).
func RunScripts(cmd, cwd string) []Script {
	if !strings.Contains(cmd, ">") && !strings.Contains(cmd, "tee") {
		return nil
	}
	segs, seps := SegmentsSep(cmd)
	return runScripts(segs, seps, cwd)
}

func runScripts(segs, seps []string, cwd string) []Script {
	written := map[string][]string{}
	var piped, out []string
	var scripts []Script
	eachSegment(segs, seps, cwd, func(i int, _ []string, dir string, lost bool) {
		words, targets := Redirects(segs[i])
		for len(words) > 0 && isShellKeyword(words[0]) {
			words = words[1:]
		}
		rest, _ := Unwrap(words, dir)
		if len(rest) == 0 {
			return
		}
		out, piped = printed(rest, piped), nil
		if filepath.Base(rest[0]) == "tee" {
			targets = append(targets, operandWords(rest[1:])...)
		}
		for _, t := range targets {
			k := scriptKey(t, dir)
			written[k] = append(written[k], out...)
		}
		if f, ok := scriptRun(rest); ok {
			for _, text := range written[scriptKey(f, dir)] {
				scripts = append(scripts, Script{Text: text, Dir: dir, Lost: lost})
			}
		}
		if seps[i] == "|" || seps[i] == "|&" {
			piped = out
		}
	})
	return scripts
}

// printed is what a segment prints: echo's text raw and with escapes decoded, printf's formatted, tee's stdin.
func printed(toks []string, stdin []string) []string {
	words := toks[1:]
	switch filepath.Base(toks[0]) {
	case "tee":
		return stdin
	case "echo":
		for len(words) > 0 && len(words[0]) > 1 && words[0][0] == '-' && strings.Trim(words[0][1:], "neE") == "" {
			words = words[1:]
		}
		raw := strings.Join(words, " ")
		return slices.Compact([]string{raw, decodeEscapes(raw)})
	case "printf":
		if len(words) > 0 && words[0] == "--" {
			words = words[1:]
		}
		if len(words) == 0 || strings.HasPrefix(words[0], "-") { // -v assigns a variable, printing nothing
			return nil
		}
		return []string{printfOutput(words[0], words[1:])}
	}
	return nil
}

// operandWords are the words of args that are not options.
func operandWords(args []string) []string {
	var out []string
	for _, a := range args {
		if a != "" && a[0] != '-' {
			out = append(out, a)
		}
	}
	return out
}

// scriptKey names the file t at dir: its path, or the word itself when the name or the dir is dynamic.
func scriptKey(t, dir string) string {
	if p, ok := resolvePath(t, dir); ok && p != unknownDir {
		return p
	}
	return t
}

// scriptRun is the file a segment runs as a script: a shell's first operand, `.`/`source`'s, or a command by path.
func scriptRun(toks []string) (string, bool) {
	name := filepath.Base(toks[0])
	switch {
	case shellWrapperNames[name]:
		if _, ok := nestedOf(toks); ok {
			return "", false
		}
		if ops := operandWords(toks[1:]); len(ops) > 0 {
			return ops[0], true
		}
		return "", false
	case (name == "." || name == "source") && len(toks) > 1:
		return toks[1], true
	}
	return toks[0], strings.Contains(toks[0], "/")
}

// printfOutput applies a printf format to args, reusing the format while args remain, as the shell does.
func printfOutput(format string, args []string) string {
	var out strings.Builder
	for {
		used := printfPass(&out, format, args)
		if args = args[used:]; used == 0 || len(args) == 0 {
			return out.String()
		}
	}
}

// printfPass writes one pass of format: its escapes decoded, each % verb an argument (`%b` decoded); returns args used.
func printfPass(out *strings.Builder, format string, args []string) int {
	used := 0
	for i := 0; i < len(format); i++ {
		switch {
		case format[i] == '\\' && i+1 < len(format):
			i += writeEscape(out, format[i+1:], false)
		case format[i] == '%' && i+1 < len(format):
			j := i + 1
			for j < len(format) && strings.IndexByte("-+ #0123456789.", format[j]) >= 0 {
				j++
			}
			i = min(j, len(format)-1)
			if j >= len(format) || format[j] == '%' {
				out.WriteByte('%')
				continue
			}
			arg := ""
			if used < len(args) {
				arg, used = args[used], used+1
			}
			if format[j] == 'b' {
				arg = decodeEscapes(arg)
			}
			out.WriteString(arg)
		default:
			out.WriteByte(format[i])
		}
	}
	return used
}

// decodeEscapes applies echo -e / printf %b escapes: `\0NNN` octal, and `\c` ends the output.
func decodeEscapes(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out.WriteByte(s[i])
			continue
		}
		if s[i+1] == 'c' {
			break
		}
		i += writeEscape(&out, s[i+1:], true)
	}
	return out.String()
}

// writeEscape writes the escape after a backslash (s) and returns its length; zeroOctal reads `\0NNN` as one byte.
func writeEscape(out *strings.Builder, s string, zeroOctal bool) int {
	if zeroOctal && s[0] == '0' {
		end := digitRun(s, 1, 3, 8)
		v, _ := strconv.ParseUint("0"+s[1:max(end, 1)], 8, 8)
		out.WriteByte(byte(v))
		return max(end, 1)
	}
	if r, n, ok := ansiEscape(s); ok {
		out.WriteString(r)
		return n
	}
	out.WriteByte('\\')
	out.WriteByte(s[0])
	return 1
}
